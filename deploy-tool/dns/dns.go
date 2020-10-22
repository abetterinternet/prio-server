package dns

import (
	"fmt"
	"strings"

	"github.com/abetterinternet/prio-server/deploy-tool/config"
	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
)

// GetACMEDNSProvider configures an ACMEDNSProvider value to be used in cert generation
func GetACMEDNSProvider(deployConfig config.DeployConfig) (certmagic.ACMEDNSProvider, error) {
	//nolint:gocritic
	switch strings.ToLower(deployConfig.DNS.Provider) {
	case "cloudflare":
		if deployConfig.DNS.CloudflareConfig == nil {
			return nil, fmt.Errorf("cloudflare configuration of the configuration was nil")
		}
		provider := &cloudflare.Provider{
			APIToken: deployConfig.DNS.CloudflareConfig.APIKey,
		}

		return provider, nil
	}

	return nil, fmt.Errorf("no valid provider selected")
}
