use anyhow::{anyhow, Context, Result};
use chrono::{prelude::Utc, DateTime, Duration};
use jsonwebtoken::{encode, Algorithm, EncodingKey, Header};
use serde::{Deserialize, Serialize};
use std::{fmt, io::Read};
use ureq::Response;

use crate::http::{send_json_request, JsonRequestParameters};

const DEFAULT_OAUTH_TOKEN_URL: &str =
    "http://metadata.google.internal:80/computeMetadata/v1/instance/service-accounts/default/token";

/// Represents the claims encoded into JWTs when using a service account key
/// file to authenticate as the default GCP service account.
#[derive(Debug, Serialize, Deserialize)]
struct Claims {
    iss: String,
    scope: String,
    aud: String,
    iat: i64,
    exp: i64,
}

/// A wrapper around an Oauth token and its expiration date.
struct OauthToken {
    token: String,
    expiration: DateTime<Utc>,
}

impl OauthToken {
    /// Returns true if the token is expired.
    fn expired(&self) -> bool {
        Utc::now() >= self.expiration
    }
}

/// Represents the response from a GET request to the GKE metadata service's
/// service account token endpoint, or to oauth2.googleapis.com.token.
/// https://developers.google.com/identity/protocols/oauth2/service-account
#[derive(Deserialize, PartialEq)]
struct OauthTokenResponse {
    access_token: String,
    expires_in: i64,
    token_type: String,
}

/// Represents the response from a POST request to the GCP IAM service's
/// generateAccessToken endpoint.
/// https://cloud.google.com/iam/docs/reference/credentials/rest/v1/projects.serviceAccounts/generateAccessToken
#[derive(Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
struct GenerateAccessTokenResponse {
    access_token: String,
    expire_time: DateTime<Utc>,
}

/// This is the subset of a GCP service account key file that we need to parse
/// to construct a signed JWT.
#[derive(Debug, Deserialize, PartialEq)]
struct ServiceAccountKeyFile {
    /// The PEM-armored base64 encoding of the ASN.1 encoding of the account's
    /// RSA private key.
    private_key: String,
    /// The private key ID.
    private_key_id: String,
    /// The service account's email address.
    client_email: String,
    /// The URL from which OAuth tokens should be requested.
    token_uri: String,
}

/// OauthTokenProvider manages a default service account Oauth token (i.e. the
/// one for a GCP service account mapped to a Kubernetes service account, or the
/// one found in a JSON key file) and an Oauth token used to impersonate another
/// service account.
pub(crate) struct OauthTokenProvider {
    /// The Oauth scope for which tokens should be requested.
    scope: String,
    /// The parsed key file for the default GCP service account. If present,
    /// this will be used to obtain the default account OAuth token. If absent,
    /// the GKE metadata service is consulted.
    default_service_account_key_file: Option<ServiceAccountKeyFile>,
    /// Holds the service account email to impersonate, if one was provided to
    /// OauthTokenProvider::new.
    account_to_impersonate: Option<String>,
    /// This field is None after instantiation and is Some after the first
    /// successful request for a token for the default service account, though
    /// the contained token may be expired.
    default_account_token: Option<OauthToken>,
    /// This field is None after instantiation and is Some after the first
    /// successful request for a token for the impersonated service account,
    /// though the contained token may be expired. This will always be None if
    /// account_to_impersonate is None.
    impersonated_account_token: Option<OauthToken>,
}

impl fmt::Debug for OauthTokenProvider {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("OauthTokenProvider")
            .field("account_to_impersonate", &self.account_to_impersonate)
            .field(
                "default_service_account_key_file",
                &self
                    .default_service_account_key_file
                    .as_ref()
                    .map(|key_file| key_file.client_email.clone()),
            )
            .field(
                "default_account_token",
                &self.default_account_token.as_ref().map(|_| "redacted"),
            )
            .field(
                "impersonated_account_token",
                &self.default_account_token.as_ref().map(|_| "redacted"),
            )
            .finish()
    }
}

impl OauthTokenProvider {
    /// Creates a token provider which can impersonate the specified service
    /// account.
    pub(crate) fn new(
        scope: &str,
        account_to_impersonate: Option<String>,
        key_file_reader: Option<Box<dyn Read>>,
    ) -> Result<OauthTokenProvider> {
        let key_file: Option<ServiceAccountKeyFile> = match key_file_reader {
            Some(reader) => {
                serde_json::from_reader(reader).context("failed to deserialize JSON key file")?
            }
            None => None,
        };
        Ok(OauthTokenProvider {
            scope: scope.to_owned(),
            default_service_account_key_file: key_file,
            account_to_impersonate,
            default_account_token: None,
            impersonated_account_token: None,
        })
    }

    /// Returns the Oauth token to use with GCP API in an Authorization header,
    /// fetching it or renewing it if necessary. If a service account to
    /// impersonate was provided, the default service account is used to
    /// authenticate to the GCP IAM API to retrieve an Oauth token. If no
    /// impersonation is taking place, provides the default service account
    /// Oauth token.
    pub(crate) fn ensure_oauth_token(&mut self) -> Result<String> {
        match self.account_to_impersonate {
            Some(_) => self.ensure_impersonated_service_account_oauth_token(),
            None => self.ensure_default_account_token(),
        }
    }

    /// Returns the current OAuth token for the default service account, if it
    /// is valid. Otherwise obtains and returns a new one.
    /// The returned value is an owned reference because the token owned by this
    /// struct could change while the caller is still holding the returned token
    fn ensure_default_account_token(&mut self) -> Result<String> {
        if let Some(token) = &self.default_account_token {
            if !token.expired() {
                return Ok(token.token.clone());
            }
        }

        let http_response = match &self.default_service_account_key_file {
            Some(key_file) => self.account_token_with_key_file(&key_file)?,
            None => OauthTokenProvider::account_token_from_gke_metadata_service(),
        };
        if http_response.error() {
            return Err(anyhow!(
                "failed to query GKE metadata service: {:?}",
                http_response
            ));
        }

        let response = http_response
            .into_json_deserialize::<OauthTokenResponse>()
            .context("failed to deserialize response from GKE metadata service")?;

        if response.token_type != "Bearer" {
            return Err(anyhow!("unexpected token type {}", response.token_type));
        }

        self.default_account_token = Some(OauthToken {
            token: response.access_token.clone(),
            expiration: Utc::now() + Duration::seconds(response.expires_in),
        });

        Ok(response.access_token)
    }

    /// Fetches default account token from GKE metadata service. Returns the
    /// ureq::Response, whose body will be an OauthTokenResponse if the HTTP
    /// call was successful, but may be an error.
    fn account_token_from_gke_metadata_service() -> Response {
        ureq::get(DEFAULT_OAUTH_TOKEN_URL)
            .set("Metadata-Flavor", "Google")
            // By default, ureq will wait forever to connect or read.
            .timeout_connect(10_000) // ten seconds
            .timeout_read(10_000) // ten seconds
            .call()
    }

    /// Fetches the default account token from Google OAuth API using a JWT
    /// constructed from the parameters in the provided key file. If the JWT is
    /// successfully constructed, returns the ureq::Response whose body will be
    /// an OauthTokenResponse if the HTTP call was successful, but may be an
    /// error.
    fn account_token_with_key_file(&self, key_file: &ServiceAccountKeyFile) -> Result<Response> {
        // We construct the JWT per Google documentation:
        // https://developers.google.com/identity/protocols/oauth2/service-account#authorizingrequests
        let mut header = Header::new(Algorithm::RS256);
        header.kid = Some(key_file.private_key_id.to_owned());

        // The iat and exp fields in a JWT are in seconds since UNIX epoch.
        let now = Utc::now().timestamp();
        let claims = Claims {
            iss: key_file.client_email.to_owned(),
            scope: self.scope.to_owned(),
            aud: key_file.token_uri.to_owned(),
            iat: now,
            exp: now + 3600, // token expires in one hour
        };

        let encoding_key = EncodingKey::from_rsa_pem(key_file.private_key.as_bytes())
            .context("failed to parse PEM RSA key")?;

        let token =
            encode(&header, &claims, &encoding_key).context("failed to construct and sign JWT")?;

        let request_body = format!(
            "grant_type={}&assertion={}",
            urlencoding::encode("urn:ietf:params:oauth:grant-type:jwt-bearer"),
            token
        );

        Ok(ureq::post(&key_file.token_uri)
            .set("Content-Type", "application/x-www-form-urlencoded")
            // By default, ureq will wait forever to connect or read.
            .timeout_connect(10_000) // ten seconds
            .timeout_read(10_000) // ten seconds
            .send_string(&request_body))
    }

    /// Returns the current OAuth token for the impersonated service account, if
    /// it is valid. Otherwise obtains and returns a new one.
    fn ensure_impersonated_service_account_oauth_token(&mut self) -> Result<String> {
        if self.account_to_impersonate.is_none() {
            return Err(anyhow!("no service account to impersonate was provided"));
        }

        if let Some(token) = &self.impersonated_account_token {
            if !token.expired() {
                return Ok(token.token.clone());
            }
        }

        let service_account_to_impersonate = self.account_to_impersonate.clone().unwrap();
        // API reference:
        // https://cloud.google.com/iam/docs/reference/credentials/rest/v1/projects.serviceAccounts/generateAccessToken
        let request_url = format!("https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/{}:generateAccessToken",
            service_account_to_impersonate);
        let request = ureq::post(&request_url)
            .set(
                "Authorization",
                &format!("Bearer {}", self.ensure_default_account_token()?),
            )
            .build();
        let http_response = send_json_request(JsonRequestParameters {
            request: request,
            body: ureq::json!({
                "scope": [self.scope]
            }),
            ..Default::default()
        })?;
        if http_response.error() {
            return Err(anyhow!(
                "failed to get Oauth token to impersonate service account {}: {:?}",
                service_account_to_impersonate,
                http_response
            ));
        }

        let response = http_response
            .into_json_deserialize::<GenerateAccessTokenResponse>()
            .context("failed to deserialize response from IAM API")?;
        self.impersonated_account_token = Some(OauthToken {
            token: response.access_token.clone(),
            expiration: response.expire_time,
        });

        Ok(response.access_token)
    }
}
