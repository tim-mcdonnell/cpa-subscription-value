package poll

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// credential is what one poll needs from an auth file. It lives only for the
// duration of a poll and must never reach a log field, setting or error.
type credential struct {
	accessToken string
	accountID   string // Codex only
	email       string
}

// parseCredential reads the auth-file JSON returned by host.auth.get. Lookup
// order follows cpa-prometheus-plugin: access_token (else token); Codex
// account id from account_id, chatgpt_account_id, then the id_token claims.
func parseCredential(raw []byte) credential {
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return credential{}
	}
	c := credential{
		accessToken: lookup(doc, "access_token", 0),
		accountID:   lookup(doc, "account_id", 0),
		email:       lookup(doc, "email", 0),
	}
	if c.accessToken == "" {
		c.accessToken = lookup(doc, "token", 0)
	}
	if c.accountID == "" {
		c.accountID = lookup(doc, "chatgpt_account_id", 0)
	}
	if c.accountID == "" {
		c.accountID = accountIDFromJWT(lookup(doc, "id_token", 0))
	}
	return c
}

const maxLookupDepth = 16

func lookup(v any, key string, depth int) string {
	if depth > maxLookupDepth {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		for _, child := range t {
			if s := lookup(child, key, depth+1); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range t {
			if s := lookup(child, key, depth+1); s != "" {
				return s
			}
		}
	}
	return ""
}

func accountIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return ""
		}
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if s, _ := claims["chatgpt_account_id"].(string); s != "" {
		return s
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if s, _ := auth["chatgpt_account_id"].(string); s != "" {
			return s
		}
	}
	return ""
}
