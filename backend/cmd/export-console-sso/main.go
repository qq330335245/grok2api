package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/security"
	_ "github.com/glebarez/sqlite"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: export-console-sso <config.yaml> <backend.db> <out.json> [limit]")
		os.Exit(2)
	}
	configPath, dbPath, outPath := os.Args[1], os.Args[2], os.Args[3]
	limit := 12
	if len(os.Args) > 4 {
		fmt.Sscanf(os.Args[4], "%d", &limit)
	}
	raw, err := os.ReadFile(configPath)
	must(err)
	re := regexp.MustCompile(`credentialEncryptionKey:\s*"([^"]+)"`)
	m := re.FindSubmatch(raw)
	if m == nil {
		re = regexp.MustCompile(`credentialEncryptionKey:\s*'([^']+)'`)
		m = re.FindSubmatch(raw)
	}
	if m == nil {
		fail("credentialEncryptionKey missing")
	}
	cipher, err := security.NewCipher(string(m[1]))
	must(err)

	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	must(err)
	defer db.Close()
	rows, err := db.Query(`
		SELECT a.id, a.name, c.encrypted_primary
		FROM provider_accounts a
		JOIN account_credentials c ON c.account_id = a.id
		WHERE a.provider = 'grok_console' AND a.enabled = 1 AND a.auth_status = 'active'
		  AND c.encrypted_primary IS NOT NULL AND c.encrypted_primary != ''
		ORDER BY a.id DESC
		LIMIT ?`, limit)
	must(err)
	defer rows.Close()

	type acc struct {
		ID       uint64 `json:"id"`
		Name     string `json:"name"`
		SSOToken string `json:"sso_token"`
	}
	var accounts []acc
	failN := 0
	for rows.Next() {
		var id uint64
		var name, enc string
		must(rows.Scan(&id, &name, &enc))
		tok, err := cipher.Decrypt(enc)
		if err != nil || strings.TrimSpace(tok) == "" {
			failN++
			continue
		}
		accounts = append(accounts, acc{ID: id, Name: name, SSOToken: tok})
	}
	must(rows.Err())
	out, _ := json.Marshal(map[string]any{"accounts": accounts})
	must(os.WriteFile(outPath, out, 0o600))
	fmt.Printf("exported=%d failures=%d out=%s\n", len(accounts), failN, outPath)
	ids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, fmt.Sprintf("%d", a.ID))
	}
	fmt.Println("ids=" + strings.Join(ids, ","))
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}
func fail(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }
