// Package authenv resolves credentials from Terraform-style ARM_* environment
// variables (with explicit overrides) into an adminapi.TokenProvider. Sharing the
// same convention lets the CLI tools, CI, and a future Terraform provider use one
// set of credentials.
//
// Recognised env vars (flags/Config fields override):
//
//	ARM_TENANT_ID, ARM_CLIENT_ID
//	ARM_CLIENT_SECRET                                  (secret flow)
//	ARM_CLIENT_CERTIFICATE_PATH, ARM_CLIENT_CERTIFICATE_PASSWORD  (cert flow)
//	ARM_USE_OIDC=true                                  (federated / workload identity)
package authenv

import (
	"fmt"
	"os"
	"strings"

	"github.com/philband/go-exoscc/adminapi"
	"github.com/philband/go-exoscc/msalauth"
)

// Config holds explicit overrides; empty fields fall back to ARM_* env vars.
type Config struct {
	Tenant        string
	ClientID      string
	Auth          string // "auto" | "federated" | "cert" | "secret"
	CertPEM       string
	CertPassword  string
	Secret        string
	AuthorityHost string
}

func orEnv(v, key string) string {
	if v != "" {
		return v
	}
	return os.Getenv(key)
}

// Build resolves the config and returns a TokenProvider, the resolved tenant, and
// the chosen auth mode.
func Build(c Config) (tp adminapi.TokenProvider, tenant, mode string, err error) {
	c.Tenant = orEnv(c.Tenant, "ARM_TENANT_ID")
	c.ClientID = orEnv(c.ClientID, "ARM_CLIENT_ID")
	c.Secret = orEnv(c.Secret, "ARM_CLIENT_SECRET")
	c.CertPEM = orEnv(c.CertPEM, "ARM_CLIENT_CERTIFICATE_PATH")
	c.CertPassword = orEnv(c.CertPassword, "ARM_CLIENT_CERTIFICATE_PASSWORD")
	if c.Tenant == "" || c.ClientID == "" {
		return nil, "", "", fmt.Errorf("authenv: tenant and client-id required (ARM_TENANT_ID / ARM_CLIENT_ID)")
	}

	mode = strings.ToLower(c.Auth)
	if mode == "" || mode == "auto" {
		switch {
		case strings.EqualFold(os.Getenv("ARM_USE_OIDC"), "true") || os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL") != "":
			mode = "federated"
		case c.Secret != "":
			mode = "secret"
		case c.CertPEM != "":
			mode = "cert"
		default:
			return nil, "", "", fmt.Errorf("authenv: no credential — set ARM_CLIENT_SECRET, ARM_CLIENT_CERTIFICATE_PATH, or ARM_USE_OIDC=true")
		}
	}

	switch mode {
	case "federated":
		tp, err = msalauth.NewConfidentialAssertion(c.Tenant, c.ClientID, msalauth.GitHubOIDCAssertion(""), c.AuthorityHost)
	case "cert":
		if c.CertPEM == "" {
			return nil, "", "", fmt.Errorf("authenv: cert mode needs ARM_CLIENT_CERTIFICATE_PATH")
		}
		var pem []byte
		if pem, err = os.ReadFile(c.CertPEM); err == nil {
			tp, err = msalauth.NewConfidentialCertPEM(c.Tenant, c.ClientID, pem, c.CertPassword, c.AuthorityHost)
		}
	case "secret":
		if c.Secret == "" {
			return nil, "", "", fmt.Errorf("authenv: secret mode needs ARM_CLIENT_SECRET")
		}
		tp, err = msalauth.NewConfidentialSecret(c.Tenant, c.ClientID, c.Secret, c.AuthorityHost)
	default:
		return nil, "", "", fmt.Errorf("authenv: unknown auth mode %q", mode)
	}
	return tp, c.Tenant, mode, err
}
