// Command fetch-spec pulls the live Admin API spec — the OData $metadata and the
// EXOModuleFile cmdlet manifest (the raw Microsoft ExchangeOnline.psm1) — for the
// selected clouds, app-only. Credentials come from ARM_* env vars (or flags); in
// CI it uses a GitHub Actions OIDC token via an Entra federated identity
// credential (ARM_USE_OIDC=true), locally a certificate or client secret.
//
// The Microsoft psm1 is written to -psm1-dir (transient, git-ignored — not
// committed for copyright reasons); the $metadata (schema) to -metadata-dir. A
// downstream step runs generator/extract-catalog.ps1 on the psm1 to (re)produce
// the committed catalog JSON, then the Go generators.
//
// App-only requires the Entra app to have Office 365 Exchange Online ->
// Exchange.ManageAsApp AND a supported directory role on its service principal;
// without the role the service returns "The role assigned to application ... isn't
// supported in this scenario".
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
	"github.com/philband/go-exoscc/internal/authenv"
)

func main() {
	var (
		cloudSel = flag.String("cloud", "EXO", "EXO | Purview | Both")
		tenant   = flag.String("tenant", "", "tenant ID/domain (default: $ARM_TENANT_ID)")
		clientID = flag.String("client-id", "", "Entra app (client) ID (default: $ARM_CLIENT_ID)")
		auth     = flag.String("auth", "auto", "auto | federated | cert | secret")
		certPEM  = flag.String("cert-pem", "", "PEM cert+key (default: $ARM_CLIENT_CERTIFICATE_PATH)")
		certPass = flag.String("cert-password", "", "PEM key password (default: $ARM_CLIENT_CERTIFICATE_PASSWORD)")
		secret   = flag.String("secret", "", "client secret (default: $ARM_CLIENT_SECRET)")
		authHost = flag.String("authority-host", "", "national-cloud login host override")
		psm1Dir  = flag.String("psm1-dir", ".spec-cache", "where to write the (transient) Microsoft psm1")
		metaDir  = flag.String("metadata-dir", "spec/metadata", "where to write $metadata")
	)
	flag.Parse()

	tp, _, mode, err := authenv.Build(authenv.Config{
		Tenant: *tenant, ClientID: *clientID, Auth: *auth,
		CertPEM: *certPEM, CertPassword: *certPass, Secret: *secret, AuthorityHost: *authHost,
	})
	check(err)
	fmt.Printf("auth=%s\n", mode)

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
	var anyResource string
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

// extractPsm1 pulls ExchangeOnline.psm1 out of the EXOModuleFile JSON envelope.
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
