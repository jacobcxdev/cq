package zai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

// VerifiedAPIKeyOwnership contains account-bound evidence, never fetched secrets.
// Only VerifyAPIKeyOwnership can construct its private fields.
type VerifiedAPIKeyOwnership struct {
	userID string
	keyIDs []string
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
