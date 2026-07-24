// Command verify is a smoke test: it drives the generated bindings against the
// live Admin API using a pre-acquired access-token JWT, proving the transport
// (regional redirect, cookie, headers) and response parsing work.
//
//	go run ./cmd/verify -token ./token.jwt
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/philband/go-exoscc/adminapi"
	"github.com/philband/go-exoscc/exo"
)

func main() {
	tokenFile := flag.String("token", "", "path to a file containing a raw EXO access-token JWT")
	flag.Parse()
	if *tokenFile == "" {
		fmt.Fprintln(os.Stderr, "verify: -token required")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*tokenFile)
	check(err)
	jwt := strings.TrimSpace(string(raw))
	tid, upn := claim(jwt, "tid"), claim(jwt, "upn")
	if upn == "" {
		upn = claim(jwt, "unique_name")
	}
	fmt.Printf("tenant=%s upn=%s\n", tid, upn)

	c, err := adminapi.New(adminapi.Options{
		Cloud:    adminapi.EXO,
		TenantID: tid,
		Tokens:   adminapi.StaticTokenProvider(jwt),
		Anchor:   "UPN:" + upn,
	})
	check(err)
	svc := exo.New(c)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := svc.GetOrganizationConfig(ctx, exo.GetOrganizationConfigParams{})
	check(err)
	fmt.Printf("Get-OrganizationConfig: %d object(s)\n", len(res.Value))
	if len(res.Value) > 0 {
		fmt.Printf("  Name=%v\n  Guid=%v\n  AdminDisplayVersion=%v\n",
			res.Value[0]["Name"], res.Value[0]["Guid"], res.Value[0]["AdminDisplayVersion"])
	}
	fmt.Printf("RateLimit: %d/%d reset=%s\n", res.RateLimit.Remaining, res.RateLimit.Limit, res.RateLimit.Reset.Format(time.RFC3339))

	// A second cmdlet that returns multiple objects, to exercise value arrays.
	ad, err := svc.GetAcceptedDomain(ctx, exo.GetAcceptedDomainParams{})
	if err != nil {
		fmt.Println("Get-AcceptedDomain:", err)
		return
	}
	fmt.Printf("Get-AcceptedDomain: %d domain(s)\n", len(ad.Value))
	for _, d := range ad.Value {
		fmt.Printf("  - %v (default=%v)\n", d["DomainName"], d["Default"])
	}
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
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(1)
	}
}
