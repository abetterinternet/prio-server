use anyhow::{Context, Result};
use std::{convert::From, default::Default, fmt::Debug, time::Duration};
use ureq::{Agent, AgentBuilder, Request, Response, SerdeValue};
use url::Url;

use crate::retries::retry_request;

/// Method contains the HTTP methods supported by this crate.
#[derive(Debug)]
pub(crate) enum Method {
    Get,
    Post,
    Put,
    Delete,
}

impl Method {
    /// Converts the enum to a primitive string to be used by the ureq::Agent
    fn to_primitive_string(&self) -> &str {
        match self {
            Method::Get => "GET",
            Method::Post => "POST",
            Method::Put => "PUT",
            Method::Delete => "DELETE",
        }
    }
}

/// An HTTP agent that can be configured to manage "Authorization" headers and
/// retries using exponential backoff.
#[derive(Debug, Clone)]
pub(crate) struct RetryingAgent {
    /// Agent to use for constructing HTTP requests.
    agent: Agent,
    /// By default, requests which fail due to transport problems or which
    /// return any 5xx HTTP code will be retried with exponential backoff. If
    /// set, these HTTP codes will also cause retries.
    additional_retryable_http_codes: Option<Vec<u16>>,
}

impl Default for RetryingAgent {
    fn default() -> Self {
        Self::new(
            AgentBuilder::new().timeout(Duration::from_secs(10)).build(),
            None,
        )
    }
}

impl RetryingAgent {
    pub fn new(agent: Agent, additional_retryable_http_codes: Option<Vec<u16>>) -> Self {
        Self {
            agent,
            additional_retryable_http_codes,
        }
    }

    /// Prepares a request for the provided `RequestParameters`. Returns a
    /// `ureq::Request` permitting the caller to further customize the request
    /// (e.g., with HTTP headers or query parameters). Callers may use methods
    /// like `send()` or `send_bytes()` directly on the returned `Request`, but
    /// must use `RetryingAgent::send_json_request`, `::send_bytes` or
    /// `::send_string` to get retries.
    /// Returns an Error if the OauthTokenProvider returns an error when
    /// supplying the request with an OauthToken.
    pub(crate) fn prepare_request(&self, parameters: RequestParameters) -> Result<Request> {
        let mut request = self
            .agent
            .request_url(parameters.method.to_primitive_string(), &parameters.url);
        if let Some(token_provider) = parameters.token_provider {
            let token = token_provider.ensure_oauth_token()?;
            request = request.set("Authorization", &format!("Bearer {}", token));
        }
        Ok(request)
    }

    fn is_http_status_retryable(&self, http_status: u16) -> bool {
        if http_status >= 500 {
            return true;
        }

        if let Some(codes) = &self.additional_retryable_http_codes {
            return codes.contains(&http_status);
        }

        false
    }

    fn is_error_retryable(&self, error: &ureq::Error) -> bool {
        match error {
            ureq::Error::Status(http_status, _) => self.is_http_status_retryable(*http_status),
            ureq::Error::Transport(_) => true,
        }
    }

    /// Send the provided request with the provided JSON body.
    pub(crate) fn send_json_request(&self, request: Request, body: SerdeValue) -> Result<Response> {
        retry_request(
            "send json request",
            || request.clone().send_json(body.clone()),
            |ureq_error| self.is_error_retryable(ureq_error),
        )
        .context("failed to send JSON request")
    }

    /// Send the provided request with the provided bytes as the body.
    pub(crate) fn send_bytes(&self, request: Request, data: &[u8]) -> Result<Response> {
        retry_request(
            "send bytes",
            || request.clone().send_bytes(data),
            |ureq_error| self.is_error_retryable(ureq_error),
        )
        .context("failed to send request with bytes body")
    }

    /// Send the provided request with the provided string as the body.
    pub(crate) fn send_string(&self, request: Request, data: &str) -> Result<Response> {
        retry_request(
            "send string",
            || request.clone().send_string(data),
            |ureq_error| self.is_error_retryable(ureq_error),
        )
        .context("failed to send request with string body")
    }

    /// Send the provided request with no body.
    pub(crate) fn call(&self, request: Request) -> Result<Response> {
        retry_request(
            "send request without body",
            || request.clone().call(),
            |ureq_error| self.is_error_retryable(ureq_error),
        )
        .context("failed to make request")
    }
}

/// Defines a behavior responsible for produing bearer authorization tokens
pub(crate) trait OauthTokenProvider: Debug {
    /// Returns a valid bearer authroization token
    fn ensure_oauth_token(&mut self) -> Result<String>;
}

/// StaticOauthTokenProvider is an OauthTokenProvider that contains a String
/// as the token. This structure implements the OauthTokenProvider trait and can
/// be used in RequestParameters.
#[derive(Debug)]
pub(crate) struct StaticOauthTokenProvider {
    pub token: String,
}

impl OauthTokenProvider for StaticOauthTokenProvider {
    fn ensure_oauth_token(&mut self) -> Result<String> {
        Ok(self.token.clone())
    }
}

impl From<String> for StaticOauthTokenProvider {
    fn from(token: String) -> Self {
        StaticOauthTokenProvider { token }
    }
}

/// Struct containing parameters for send_json_request
#[derive(Debug)]
pub(crate) struct RequestParameters<'a> {
    /// The url to request
    pub url: Url,
    /// The method of the request (GET, POST, etc)
    pub method: Method,
    /// If this field is set, the request with be sent with an "Authorization"
    /// header containing a bearer token obtained from the OauthTokenProvider.
    /// If unset, the request is sent unauthenticated.
    pub token_provider: Option<&'a mut dyn OauthTokenProvider>,
}

impl Default for RequestParameters<'_> {
    fn default() -> Self {
        let default_url = Url::parse("https://example.com").expect("example url did not parse");

        RequestParameters {
            url: default_url,
            method: Method::Get,
            token_provider: None,
        }
    }
}

/// simple_get_request does a HTTP request to a URL and returns the body as a
// string.
pub(crate) fn simple_get_request(url: Url) -> Result<String> {
    let agent = RetryingAgent::default();
    let request = agent
        .prepare_request(RequestParameters {
            url,
            method: Method::Get,
            ..Default::default()
        })
        .context("creating simple_get_request failed")?;

    agent
        .call(request)?
        .into_string()
        .context("failed to convert GET response body into string")
}

#[cfg(test)]
mod tests {
    use super::*;
    use mockito::{mock, Matcher};

    #[test]
    fn retryable_error() {
        let http_400 = ureq::Error::Status(400, Response::new(400, "", "").unwrap());
        let http_429 = ureq::Error::Status(429, Response::new(429, "", "").unwrap());
        let http_500 = ureq::Error::Status(500, Response::new(500, "", "").unwrap());
        let http_503 = ureq::Error::Status(503, Response::new(503, "", "").unwrap());
        // There is currently no way to create a ureq::Error::Transport so we
        // settle for testing different HTTP status codes.
        // https://github.com/algesten/ureq/issues/373

        let mut agent = RetryingAgent::default();
        assert!(!agent.is_error_retryable(&http_400));
        assert!(!agent.is_error_retryable(&http_429));
        assert!(agent.is_error_retryable(&http_500));
        assert!(agent.is_error_retryable(&http_503));

        agent.additional_retryable_http_codes = Some(vec![429]);

        assert!(!agent.is_error_retryable(&http_400));
        assert!(agent.is_error_retryable(&http_429));
        assert!(agent.is_error_retryable(&http_500));
        assert!(agent.is_error_retryable(&http_503));
    }

    #[test]
    fn authenticated_request() {
        let mocked_get = mock("GET", "/resource")
            .match_header("Authorization", "Bearer fake-token")
            .with_status(200)
            .with_body("fake body")
            .expect_at_most(1)
            .create();

        let mut oauth_token_provider = StaticOauthTokenProvider {
            token: "fake-token".to_string(),
        };

        let request_parameters = RequestParameters {
            url: Url::parse(&format!("{}/resource", mockito::server_url())).unwrap(),
            method: Method::Get,
            token_provider: Some(&mut oauth_token_provider),
        };

        let agent = RetryingAgent::default();
        let request = agent.prepare_request(request_parameters).unwrap();

        let response = agent.call(request).unwrap();

        mocked_get.assert();

        assert_eq!(response.status(), 200);
        assert_eq!(response.into_string().unwrap(), "fake body");
    }

    #[test]
    fn unauthenticated_request() {
        let mocked_get = mock("GET", "/resource")
            .match_header("Authorization", Matcher::Missing)
            .with_status(200)
            .with_body("fake body")
            .expect_at_most(1)
            .create();

        let request_parameters = RequestParameters {
            url: Url::parse(&format!("{}/resource", mockito::server_url())).unwrap(),
            method: Method::Get,
            token_provider: None,
        };

        let agent = RetryingAgent::default();
        let request = agent.prepare_request(request_parameters).unwrap();

        let response = agent.call(request).unwrap();

        mocked_get.assert();

        assert_eq!(response.status(), 200);
        assert_eq!(response.into_string().unwrap(), "fake body");
    }
}
