package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const clientConfigsURL = "https://zcode.z.ai/api/v1/client/configs"
const planOffersURL = "https://zcode.z.ai/api/v1/marketing/touch"
const planClaimURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/claim"

var marketingRequestSeq atomic.Uint64

// VerificationConfig is the server's current public CAPTCHA configuration.
// SkipModelRequest concerns inference only; it never skips claim verification.
type VerificationConfig struct {
	Enabled          bool   `json:"enabled"`
	Prefix           string `json:"prefix"`
	Region           string `json:"region"`
	SceneID          string `json:"sceneId"`
	SkipModelRequest bool   `json:"skip_model_request"`
}

// VerificationProof contains the opaque result of a real SDK success callback.
// Boundary validation cannot establish its authenticity; Z.ai validates it.
// Do not persist, log or reuse this short-lived result.
type VerificationProof struct {
	Param  string `json:"-"`
	Region string `json:"-"`
}

type PlanOffer struct {
	PlanID      string `json:"plan_id"`
	CampaignID  string `json:"campaign_id"`
	Position    string `json:"resource_position"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Priority    int    `json:"priority"`
	// Details preserves public visual args as supplied. They are display data,
	// not a structured promise of token quantities, eligibility or activation.
	Details json.RawMessage `json:"details,omitempty"`
}

type ClaimResult struct {
	ServerTime json.RawMessage `json:"server_time,omitempty"`
	Plan       Plan            `json:"plan"`
}

// CaptchaConfig reads only public configuration. It never starts verification.
func (c Client) CaptchaConfig(ctx context.Context) (VerificationConfig, error) {
	data, err := c.claimRequest(ctx, http.MethodGet, versionedClaimURL(clientConfigsURL, c.Version), nil, nil, nil)
	if err != nil {
		return VerificationConfig{}, err
	}
	var decoded struct {
		Configs struct {
			Captcha *struct {
				Enabled          json.RawMessage `json:"enabled"`
				Prefix           string          `json:"prefix"`
				Region           string          `json:"region"`
				SceneID          string          `json:"sceneId"`
				SkipModelRequest json.RawMessage `json:"skip_model_request"`
			} `json:"captcha"`
		} `json:"configs"`
	}
	if json.Unmarshal(data, &decoded) != nil || decoded.Configs.Captcha == nil {
		return VerificationConfig{}, &OperationError{Code: "invalid_verification_config"}
	}
	raw := decoded.Configs.Captcha
	for _, boolean := range []json.RawMessage{raw.Enabled, raw.SkipModelRequest} {
		if len(boolean) > 0 && string(boolean) != "true" && string(boolean) != "false" {
			return VerificationConfig{}, &OperationError{Code: "invalid_verification_config"}
		}
	}
	cfg := VerificationConfig{Enabled: string(raw.Enabled) != "false", Prefix: raw.Prefix, Region: raw.Region, SceneID: raw.SceneID, SkipModelRequest: string(raw.SkipModelRequest) == "true"}
	if cfg.Enabled && !cfg.valid() {
		return VerificationConfig{}, &OperationError{Code: "invalid_verification_config"}
	}
	return cfg, nil
}

// PlanOffers reads current claim_zcode_plan actions from Marketing Touch. The
// earlier billing/preview banner is retired. No assets are fetched, actions
// reported, plans activated, or eligibility inferred from client config.
func (c Client) PlanOffers(ctx context.Context, session Session) ([]PlanOffer, error) {
	if !marketingDeviceID(c.DeviceMid) {
		return nil, &OperationError{Code: "device_unavailable"}
	}
	endpoint := planOffersURL + "?seq=" + strconv.FormatUint(marketingRequestSeq.Add(1)-1, 10)
	data, err := c.claimRequest(ctx, http.MethodGet, endpoint, &session, nil, nil)
	if err != nil {
		return nil, err
	}
	var decoded struct {
		ServerTime json.RawMessage    `json:"server_time"`
		Language   string             `json:"language"`
		Deliveries *[]json.RawMessage `json:"deliveries"`
	}
	if json.Unmarshal(data, &decoded) != nil || decoded.Deliveries == nil || len(*decoded.Deliveries) > 32 || !optionalClaimTimestamp(decoded.ServerTime) || len(decoded.ServerTime) == 0 || decoded.Language != "en-US" && decoded.Language != "zh-CN" {
		return nil, &OperationError{Code: "invalid_plan_offers"}
	}
	offers := make([]PlanOffer, 0)
	positions := make(map[string]bool)
	plans := make(map[string]bool)
	for _, delivery := range *decoded.Deliveries {
		candidates, position := marketingClaimOffers(delivery)
		if position == "" || positions[position] {
			continue
		}
		positions[position] = true
		for _, offer := range candidates {
			if !plans[offer.PlanID] {
				offers = append(offers, offer)
				plans[offer.PlanID] = true
			}
		}
	}
	return offers, nil
}

type marketingText struct {
	Format  string `json:"format"`
	Content string `json:"content"`
}

func marketingClaimOffers(raw json.RawMessage) ([]PlanOffer, string) {
	var delivery struct {
		CampaignID string          `json:"campaign_id"`
		Priority   *int            `json:"priority"`
		Position   string          `json:"resource_position"`
		Banner     json.RawMessage `json:"banner"`
		Popup      json.RawMessage `json:"popup"`
	}
	if json.Unmarshal(raw, &delivery) != nil || !marketingID(delivery.CampaignID) || delivery.Priority == nil || *delivery.Priority < 0 || *delivery.Priority > 100 {
		return nil, ""
	}
	content := delivery.Popup
	maxButtons := 4
	if delivery.Position == "banner" {
		content, maxButtons = delivery.Banner, 2
	} else if delivery.Position != "popup" {
		return nil, ""
	}
	var body struct {
		Layout      string          `json:"layout"`
		Title       *marketingText  `json:"title"`
		Description *marketingText  `json:"description"`
		Hero        json.RawMessage `json:"hero"`
		Background  json.RawMessage `json:"background"`
		Buttons     *[]struct {
			Text   marketingText `json:"text"`
			Action struct {
				Type string `json:"type"`
				Args struct {
					PlanID string `json:"plan_id"`
				} `json:"args"`
			} `json:"action"`
		} `json:"buttons"`
	}
	if len(content) == 0 || content[0] != '{' || json.Unmarshal(content, &body) != nil || body.Layout != "" && body.Layout != "v1" || body.Buttons == nil || len(*body.Buttons) > maxButtons {
		return nil, ""
	}
	validText := func(text *marketingText, banner bool) bool {
		return text != nil && len(text.Content) <= 20000 && (text.Format == "plaintext" || text.Format == "html" || text.Format == "markdown" || banner && text.Format == "")
	}
	visual := body.Hero
	if delivery.Position == "popup" {
		if !validText(body.Title, false) || !validText(body.Description, false) {
			return nil, ""
		}
	} else {
		visual = body.Background
	}
	var details struct {
		Type string          `json:"type"`
		Args json.RawMessage `json:"args"`
	}
	if len(visual) > 0 && string(visual) != "null" {
		if json.Unmarshal(visual, &details) != nil || details.Type != "image" && details.Type != "video" && details.Type != "bundle" || len(details.Args) > 64000 || len(details.Args) > 0 && details.Args[0] != '{' {
			return nil, ""
		}
	} else if delivery.Position == "banner" {
		return nil, ""
	}
	offers := make([]PlanOffer, 0)
	closeButtons, otherButtons := 0, 0
	for _, button := range *body.Buttons {
		if !validText(&button.Text, delivery.Position == "banner") {
			return nil, ""
		}
		switch button.Action.Type {
		case "close":
			closeButtons++
		case "open_url", "navigate", "copy_text", "claim_zcode_plan":
			otherButtons++
		default:
			return nil, ""
		}
		if delivery.Position == "banner" && (closeButtons > 1 || otherButtons > 1) {
			return nil, ""
		}
		if button.Action.Type != "claim_zcode_plan" {
			continue
		}
		if !marketingID(button.Action.Args.PlanID) {
			return nil, ""
		}
		offer := PlanOffer{PlanID: button.Action.Args.PlanID, CampaignID: delivery.CampaignID, Position: delivery.Position, Name: button.Text.Content, Priority: *delivery.Priority, Details: details.Args}
		if delivery.Position == "popup" {
			offer.Name = body.Title.Content
			offer.Description = body.Description.Content
		}
		offers = append(offers, offer)
	}
	return offers, delivery.Position
}

func marketingID(value string) bool { return len(value) <= 128 && validClaimPlanID(value) }

func marketingDeviceID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ClaimPlan makes exactly one explicit claim attempt. The caller selects an ID
// from current PlanOffers and supplies a newly completed verification result.
// It does not fetch offers, repeat verification or retry a claim automatically.
func (c Client) ClaimPlan(ctx context.Context, session Session, offeredPlanID string, proof VerificationProof) (ClaimResult, error) {
	if !validClaimPlanID(offeredPlanID) {
		return ClaimResult{}, &OperationError{Code: "invalid_plan_id"}
	}
	if !proof.valid() {
		return ClaimResult{}, &OperationError{Code: "invalid_verification_proof"}
	}
	body, _ := json.Marshal(struct {
		PlanID string `json:"plan_id"`
	}{offeredPlanID})
	data, err := c.claimRequest(ctx, http.MethodPost, planClaimURL, &session, &proof, body)
	if err != nil {
		return ClaimResult{}, err
	}
	var result ClaimResult
	var object struct {
		Plan json.RawMessage `json:"plan"`
	}
	if json.Unmarshal(data, &object) != nil || len(object.Plan) == 0 || object.Plan[0] != '{' || json.Unmarshal(data, &result) != nil || !optionalClaimTimestamp(result.ServerTime) || !(Balance{Plans: []Plan{result.Plan}}).validScalars() {
		return ClaimResult{}, &OperationError{Code: "invalid_claim_result", Ambiguous: true}
	}
	return result, nil
}

func versionedClaimURL(endpoint, version string) string {
	return endpoint + "?" + url.Values{"app_version": {version}, "platform": {runtime.GOOS + "-" + runtime.GOARCH}}.Encode()
}

func (c Client) claimRequest(ctx context.Context, method, endpoint string, session *Session, proof *VerificationProof, body []byte) ([]byte, error) {
	if c.HTTP == nil || !validHeaderValue(c.Version, 80) || !validHeaderValue(c.DeviceMid, 256) {
		return nil, &OperationError{Code: "client_unavailable"}
	}
	if session != nil && (session.Family != "zai" || !validHeaderValue(session.ZCodeJWT, 16<<10)) {
		return nil, &OperationError{Code: "session_unavailable"}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &OperationError{Code: "invalid_request"}
	}
	SetSourceHeaders(req.Header, c.Version)
	req.Header.Set("X-App-Id", "zcode")
	req.Header.Set("X-Client-Version", c.Version)
	req.Header.Set("X-Device-Mid", c.DeviceMid)
	req.Header.Set("X-Platform", runtime.GOOS+"-"+runtime.GOARCH)
	req.Header.Set("X-Client-Language", "en-US")
	req.Header.Set("Accept", "application/json")
	if session != nil {
		req.Header.Set("Authorization", "Bearer "+session.ZCodeJWT)
	}
	if proof != nil {
		req.Header.Set("X-Aliyun-Captcha-Verify-Param", proof.Param)
		req.Header.Set("X-Aliyun-Captcha-Verify-Region", proof.Region)
		req.Header.Set("Content-Type", "application/json")
	}
	doer := c.HTTP
	if client, ok := doer.(*http.Client); ok {
		private := *client
		private.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		doer = &private
	}
	response, err := doer.Do(req)
	mutation := method == http.MethodPost
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, &OperationError{Code: "request_failed", Ambiguous: mutation}
	}
	if response == nil || response.Body == nil {
		return nil, &OperationError{Code: "invalid_response", Ambiguous: mutation}
	}
	defer response.Body.Close()
	raw, err := httputil.ReadBody(response.Body)
	if err != nil {
		return nil, &OperationError{Code: "read_failed", Ambiguous: mutation}
	}
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if response.StatusCode != http.StatusOK {
		failure := &OperationError{Code: "http_error", HTTPStatus: response.StatusCode, Ambiguous: mutation && (response.StatusCode == 408 || response.StatusCode >= 500)}
		if json.Unmarshal(raw, &envelope) == nil {
			if code, ok := claimResponseCode(envelope.Code); ok && code != 0 {
				failure.Code, failure.VendorCode = "api_error", code
			}
		}
		return nil, failure
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, &OperationError{Code: "invalid_envelope", Ambiguous: mutation}
	}
	code, ok := claimResponseCode(envelope.Code)
	if !ok {
		return nil, &OperationError{Code: "invalid_envelope", Ambiguous: mutation}
	}
	if code != 0 || envelope.Success != nil && !*envelope.Success {
		return nil, &OperationError{Code: "api_error", VendorCode: code, Ambiguous: mutation && code == 0}
	}
	envelope.Data = bytes.TrimSpace(envelope.Data)
	if len(envelope.Data) == 0 || envelope.Data[0] != '{' {
		return nil, &OperationError{Code: "invalid_envelope", Ambiguous: mutation}
	}
	return envelope.Data, nil
}

func claimResponseCode(raw json.RawMessage) (int, bool) {
	text := string(raw)
	if len(raw) > 0 && raw[0] == '"' {
		if json.Unmarshal(raw, &text) != nil {
			return 0, false
		}
	}
	if text == "" {
		return 0, false
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	value, err := strconv.Atoi(text)
	return value, err == nil
}

func validClaimPlanID(id string) bool {
	if id == "" || len(id) > 256 || strings.TrimSpace(id) != id {
		return false
	}
	for _, c := range id {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

func optionalClaimTimestamp(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	_, ok := unixSeconds(raw)
	return ok
}

func (p VerificationProof) valid() bool {
	if p.Region != "cn" && p.Region != "sgp" || strings.TrimSpace(p.Param) == "" || len(p.Param) > 16<<10 {
		return false
	}
	for _, c := range p.Param {
		if c < ' ' || c >= 127 {
			return false
		}
	}
	return true
}

func (c VerificationConfig) valid() bool {
	if !c.Enabled || c.Region != "cn" && c.Region != "sgp" {
		return false
	}
	for _, value := range []string{c.Prefix, c.SceneID} {
		if len(value) == 0 || len(value) > 128 {
			return false
		}
		for _, ch := range value {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
				return false
			}
		}
	}
	return true
}
