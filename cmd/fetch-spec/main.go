// Command fetch-spec pulls the live Admin API spec — the OData $metadata and the
// EXOModuleFile cmdlet manifest (the raw Microsoft ExchangeOnline.psm1) — for the
// selected clouds, using app-only auth. In CI it authenticates with a GitHub
// Actions OIDC token via an Entra federated identity credential (no secret);
// locally it can use a certificate or client secret.
//
// The Microsoft psm1 is written to -psm1-dir (transient, git-ignored — not
// committed for copyright reasons); the $metadata (schema) is written to
// -metadata-dir. A downstream step runs generator/extract-catalog.ps1 on the psm1
// to (re)produce the committed catalog JSON, then the Go generators.
//
// Note: app-only reliably reaches the Exchange Online endpoint. The Purview
// (compliance) backend routing may still resolve app-only calls to the Exchange
// admin backend (see the private research notes); validate -cloud Purview against
// your tenant. If it returns the wrong cmdlet set, keep the committed Purview
// catalog (refreshed via delegated capture).
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/philband/go-exoscc/adminapi"
	"github.com/philband/go-exoscc/msalauth"
)

func main() {
	var (
		cloudSel     = flag.String("cloud", "EXO", "EXO | Purview | Both")
		tenant       = flag.String("tenant", "", "tenant ID/domain (authority); token tid is used for the API path")
		clientID     = flag.String("client-id", "", "Entra application (client) ID")
		auth         = flag.String("auth", "federated", "federated | cert | secret")
		certPEM      = flag.String("cert-pem", "", "path to PEM (cert+key) for -auth cert")
		certPassword = flag.String("cert-password", "", "PEM key password (optional)")
		secret       = flag.String("secret", "", "client secret for -auth secret (or env EXOSCC_CLIENT_SECRET)")
		authHost     = flag.String("authority-host", "", "national-cloud login host override")
		psm1Dir      = flag.String("psm1-dir", ".spec-cache", "where to write the (transient) Microsoft psm1")
		metaDir      = flag.String("metadata-dir", "spec/metadata", "where to write $metadata")
	)
	flag.Parse()
	if *tenant == "" || *clientID == "" {
		fail("fetch-spec: -tenant and -client-id are required")
	}
	if *secret == "" {
		*secret = os.Getenv("EXOSCC_CLIENT_SECRET")
	}

	tp, err := buildProvider(*auth, *tenant, *clientID, *certPEM, *certPassword, *secret, *authHost)
	check(err)

	clouds := map[string]adminapi.Cloud{}
	switch strings.ToLower(*cloudSel) {
	case "exo":
		clouds["EXO"] = adminapi.EXO
	case "purview":
		clouds["Purview"] = adminapi.SCC
	case "both":
		clouds["EXO"] = adminapi.EXO
		clouds["Purview"] = adminapi.SCC
	default:
		fail("fetch-spec: -cloud must be EXO | Purview | Both")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Resolve the tenant GUID from a token (the Admin API path needs the tid claim).
	anyResource := adminapi.EXO.Resource
	for _, c := range clouds {
		anyResource = c.Resource
		break
	}
	tok, err := tp.Token(ctx, anyResource)
	check(err)
	tid := claim(tok, "tid")
	if tid == "" {
		fail("fetch-spec: could not read tid from token")
	}
	fmt.Printf("tenant=%s clouds=%s\n", tid, *cloudSel)

	check(os.MkdirAll(*psm1Dir, 0o755))
	check(os.MkdirAll(*metaDir, 0o755))

	for name, cloud := range clouds {
		cl, err := adminapi.New(adminapi.Options{Cloud: cloud, TenantID: tid, Tokens: tp, Anchor: "TID:" + tid})
		check(err)

		md, err := cl.FetchAdmin(ctx, "/adminapi/beta/{tid}/$metadata", false)
		check(err)
		mdPath := filepath.Join(*metaDir, name+"-metadata.xml")
		check(os.WriteFile(mdPath, md, 0o644))
		fmt.Printf("%s: %s (%d bytes)\n", name, mdPath, len(md))

		mf, err := cl.FetchAdmin(ctx, "/AdminApi/v1.0/{tid}/EXOModuleFile", true)
		check(err)
		psm1, err := extractPsm1(mf)
		check(err)
		psPath := filepath.Join(*psm1Dir, name+"-ExchangeOnline.psm1")
		check(os.WriteFile(psPath, []byte(psm1), 0o644))
		n := len(regexp.MustCompile(`(?m)^function script:[A-Za-z]+-[A-Za-z0-9]+`).FindAllString(psm1, -1))
		fmt.Printf("%s: %s (%d cmdlets)\n", name, psPath, n)
	}
}

func buildProvider(auth, tenant, clientID, certPEM, certPassword, secret, authHost string) (adminapi.TokenProvider, error) {
	switch strings.ToLower(auth) {
	case "federated":
		return msalauth.NewConfidentialAssertion(tenant, clientID, msalauth.GitHubOIDCAssertion(""), authHost)
	case "cert":
		if certPEM == "" {
			return nil, fmt.Errorf("-cert-pem required for -auth cert")
		}
		pem, err := os.ReadFile(certPEM)
		if err != nil {
			return nil, err
		}
		return msalauth.NewConfidentialCertPEM(tenant, clientID, pem, certPassword, authHost)
	case "secret":
		if secret == "" {
			return nil, fmt.Errorf("-secret (or EXOSCC_CLIENT_SECRET) required for -auth secret")
		}
		return msalauth.NewConfidentialSecret(tenant, clientID, secret, authHost)
	default:
		return nil, fmt.Errorf("unknown -auth %q", auth)
	}
}

// extractPsm1 pulls the ExchangeOnline.psm1 fileContent out of the EXOModuleFile
// JSON envelope: {"value":[{"fileName":"ExchangeOnline.psm1","fileContent":"..."}]}.
func extractPsm1(body []byte) (string, error) {
	var env struct {
		Value []struct {
			FileName    string `json:"fileName"`
			FileContent string `json:"fileContent"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("EXOModuleFile decode: %w", err)
	}
	for _, f := range env.Value {
		if f.FileName == "ExchangeOnline.psm1" {
			return f.FileContent, nil
		}
	}
	return "", fmt.Errorf("ExchangeOnline.psm1 not found in EXOModuleFile response")
}

func claim(jwt, name string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	if v, ok := m[name].(string); ok {
		return v
	}
	return ""
}

func check(err error) {
	if err != nil {
		fail("fetch-spec: " + err.Error())
	}
}
func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
