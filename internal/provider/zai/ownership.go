package zai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

// VerifiedAPIKeyOwnership contains account-bound evidence, never fetched secrets.
// Only VerifyAPIKeyOwnership can construct its private fields.
type VerifiedAPIKeyOwnership struct {
	userID string
	keyIDs []string
}

// RefreshAccounts verifies unresolved sources during an explicit reporting
// refresh. Request-time ReadAccounts remains local and read-only.
func RefreshAccounts(ctx context.Context, client httputil.Doer, fsys fsutil.FileSystem, env func(string) string, configDir string) ([]Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	accounts, err := readAccountSources(fsys, env, configDir)
	if err != nil {
		return nil, err
	}
	var candidates []Account
	warn := false
	for _, account := range accounts {
		if account.OAuth == nil && account.APIKey != "" {
			candidates = append(candidates, account)
		}
		if account.Source == "codexbar" && account.APIKey == "" {
			warn = true
		}
	}
	// Identity reads belong to reporting, never the request-time inventory.
	// Leave time for the actual quota requests if a source is unavailable.
	verifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, account := range accounts {
		if verifyCtx.Err() != nil {
			break
		}
		if account.OAuth == nil || len(candidates) == 0 {
			continue
		}
		session := *account.OAuth
		if session.BusinessExpiresAt > 0 && session.BusinessExpiresAt <= time.Now().Unix() {
			session, err = (zcode.OAuthClient{HTTP: client, Version: zcode.ClientVersion}).RefreshBusiness(verifyCtx, session)
			if err != nil {
				warn = true
				continue
			}
		}
		proof, proofErr := VerifyAPIKeyOwnership(verifyCtx, client, session, candidates)
		if proofErr != nil {
			warn = true
			continue
		}
		if len(proof.keyIDs) == 0 {
			continue
		}
		if verifyCtx.Err() != nil {
			break
		}
		if err := storeVerifiedAliases(fsys, configDir, proof); err != nil {
			warn = true
			continue
		}
		remaining := candidates[:0]
		for _, candidate := range candidates {
			if !containsAlias(proof.keyIDs, candidate.ID) {
				remaining = append(remaining, candidate)
			}
		}
		candidates = remaining
	}
	for _, candidate := range candidates {
		if candidate.Source == "codexbar" {
			warn = true
		}
	}
	if warn {
		fmt.Fprintln(os.Stderr, "cq: Z.ai: credential identity verification incomplete; unresolved CodexBar sources are excluded from accounts")
	}
	return ReadAccounts(fsys, env, configDir)
}

func storeVerifiedAliases(fsys fsutil.FileSystem, configDir string, proof VerifiedAPIKeyOwnership) error {
	if proof.userID == "" {
		return errors.New("Z.ai key ownership account unavailable")
	}
	// Re-read after network work. Preserve current preference, routes, keys and
	// session references; only append proved fingerprints to an existing owner.
	accounts, err := readOwnedAccounts(fsys, func(string) string { return "" }, configDir)
	if err != nil {
		return err
	}
	for i := range accounts {
		if accounts[i].OAuth == nil || accounts[i].OAuth.UserID != proof.userID {
			continue
		}
		changed := false
		for _, id := range proof.keyIDs {
			if !containsAlias(accounts[i].verifiedAPIKeyIDs, id) {
				accounts[i].verifiedAPIKeyIDs = append(accounts[i].verifiedAPIKeyIDs, id)
				changed = true
			}
		}
		if changed {
			return writeAccounts(fsys, configDir, accounts)
		}
		return nil
	}
	return errors.New("Z.ai key ownership account no longer configured")
}

// VerifyAPIKeyOwnership reads OAuth-owned key metadata and compares full keys.
// Key prefixes only select copy reads; they never establish ownership alone.
func VerifyAPIKeyOwnership(ctx context.Context, client httputil.Doer, session zcode.LoginSession, accounts []Account) (VerifiedAPIKeyOwnership, error) {
	proof := VerifiedAPIKeyOwnership{userID: session.UserID}
	candidates := map[string][]Account{}
	for _, a := range accounts {
		parts := strings.Split(a.APIKey, ".")
		if a.OAuth == nil && len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			candidates[parts[0]] = append(candidates[parts[0]], a)
		}
	}
	if len(candidates) == 0 {
		return proof, nil
	}
	if client == nil || session.UserID == "" || session.BusinessJWT == "" || strings.ContainsAny(session.BusinessJWT, "\r\n") {
		return VerifiedAPIKeyOwnership{}, errors.New("Z.ai key ownership verification unavailable")
	}
	if concrete, ok := client.(*http.Client); ok {
		copy := *concrete
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &copy
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	get := func(path, authorization string) (json.RawMessage, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.z.ai"+path, nil)
		if err != nil {
			return nil, errors.New("Z.ai key ownership request invalid")
		}
		req.Header.Set("Authorization", authorization)
		req.Header.Set("Accept", "application/json")
		rsp, err := client.Do(req)
		if err != nil || rsp == nil {
			return nil, errors.New("Z.ai key ownership request failed")
		}
		if rsp.Body == nil {
			return nil, errors.New("Z.ai key ownership response invalid")
		}
		defer rsp.Body.Close()
		if rsp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Z.ai key ownership verification rejected (HTTP %d)", rsp.StatusCode)
		}
		raw, err := httputil.ReadBody(rsp.Body)
		if err != nil {
			return nil, errors.New("Z.ai key ownership response invalid")
		}
		defer clear(raw)
		var envelope struct {
			Code json.RawMessage `json:"code"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &envelope) != nil {
			return nil, errors.New("Z.ai key ownership response invalid")
		}
		code := strings.Trim(string(envelope.Code), `"`)
		if (code != "0" && code != "200") || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return nil, errors.New("Z.ai key ownership response rejected")
		}
		return envelope.Data, nil
	}
	data, err := get("/api/biz/customer/getCustomerInfo", session.BusinessJWT)
	if err != nil {
		return VerifiedAPIKeyOwnership{}, err
	}
	var customer struct {
		Organizations []struct {
			ID       string `json:"organizationId"`
			Projects []struct {
				ID   string          `json:"projectId"`
				Type json.RawMessage `json:"projectType"`
			} `json:"projects"`
		} `json:"organizations"`
	}
	err = json.Unmarshal(data, &customer)
	clear(data)
	if err != nil || customer.Organizations == nil {
		return VerifiedAPIKeyOwnership{}, errors.New("Z.ai key ownership customer invalid")
	}
	reads := 0
	seen := map[string]bool{}
	for _, org := range customer.Organizations {
		for _, project := range org.Projects {
			// Only the explicit personal project variant is eligible. Missing,
			// future or team types cannot establish a personal quota identity.
			if strings.Trim(string(project.Type), `" `) != "1" || org.ID == "" || project.ID == "" {
				continue
			}
			reads++
			if reads > 32 {
				return VerifiedAPIKeyOwnership{}, errors.New("Z.ai key ownership project limit exceeded")
			}
			path := "/api/biz/v1/organization/" + url.PathEscape(org.ID) + "/projects/" + url.PathEscape(project.ID) + "/api_keys"
			data, err = get(path, "Bearer "+session.BusinessJWT)
			if err != nil {
				return VerifiedAPIKeyOwnership{}, err
			}
			var keys []struct {
				ID string `json:"apiKey"`
			}
			err = json.Unmarshal(data, &keys)
			clear(data)
			if err != nil || keys == nil {
				return VerifiedAPIKeyOwnership{}, errors.New("Z.ai key ownership key list invalid")
			}
			for _, key := range keys {
				matched := candidates[key.ID]
				if len(matched) == 0 {
					continue
				}
				reads++
				if reads > 64 {
					return VerifiedAPIKeyOwnership{}, errors.New("Z.ai key ownership read limit exceeded")
				}
				data, err = get(path+"/copy/"+url.PathEscape(key.ID), "Bearer "+session.BusinessJWT)
				if err != nil {
					return VerifiedAPIKeyOwnership{}, err
				}
				var secret struct {
					Value string `json:"secretKey"`
				}
				err = json.Unmarshal(data, &secret)
				clear(data)
				if err != nil || secret.Value == "" {
					return VerifiedAPIKeyOwnership{}, errors.New("Z.ai key ownership secret response invalid")
				}
				complete := key.ID + "." + secret.Value
				for _, a := range matched {
					id := newAccount(a.APIKey, "").ID
					if a.APIKey == complete && !seen[id] {
						proof.keyIDs = append(proof.keyIDs, id)
						seen[id] = true
					}
				}
				secret.Value = ""
				complete = ""
			}
		}
	}
	return proof, nil
}
