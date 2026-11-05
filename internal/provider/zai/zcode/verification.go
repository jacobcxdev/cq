package zcode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"mime"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const verificationSDKURL = "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"
const verificationTimeout = 120 * time.Second

type VerificationError struct{ Code string }

func (e *VerificationError) Error() string { return "ZCode verification failed: " + e.Code }

type verificationResult struct {
	proof VerificationProof
	err   error
}

// Verify opens a visible, single-use browser verification page. The caller must
// invoke it only for an explicit user action. The opener must respect ctx.
// The page receives public configuration only, never an account token. Z.ai's
// acceptance of the loopback page origin still requires human qualification.
func Verify(ctx context.Context, config VerificationConfig, openURL func(context.Context, string) error) (proof VerificationProof, err error) {
	if !config.valid() || openURL == nil {
		return VerificationProof{}, &VerificationError{Code: "invalid_config"}
	}
	if ctx.Err() != nil {
		return VerificationProof{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, verificationTimeout)
	defer cancel()
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return VerificationProof{}, &VerificationError{Code: "random_failed"}
	}
	state := hex.EncodeToString(random[:])
	clear(random[:])
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return VerificationProof{}, &VerificationError{Code: "listen_failed"}
	}
	origin := "http://" + listener.Addr().String()
	pagePath := "/verify/" + state
	results := make(chan verificationResult, 1)
	var mu sync.Mutex
	completed := false
	finish := func(result verificationResult) bool {
		mu.Lock()
		defer mu.Unlock()
		if completed {
			return false
		}
		completed = true
		results <- result
		return true
	}
	page, err := renderVerificationPage(config, state)
	if err != nil {
		_ = listener.Close()
		return VerificationProof{}, &VerificationError{Code: "page_failed"}
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				finish(verificationResult{err: &VerificationError{Code: "callback_failed"}})
				http.Error(w, "Verification failed.", http.StatusInternalServerError)
			}
		}()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Trust this page's nonce-bearing script and the official SDK's
		// dynamically loaded, versioned descendants.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+state+"' 'strict-dynamic'; connect-src 'self' https:; img-src data: https:; style-src 'unsafe-inline' https:; frame-src https:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		if r.Host != listener.Addr().String() {
			http.Error(w, "Invalid host.", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == pagePath && r.URL.RawQuery == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(page)
			return
		}
		if r.URL.Path != "/result" || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "Invalid method.", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Origin") != origin || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CQ-Verification-State")), []byte(state)) != 1 || r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			http.Error(w, "Invalid verification request.", http.StatusForbidden)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "Expected JSON.", http.StatusUnsupportedMediaType)
			return
		}
		body, err := httputil.ReadBody(io.LimitReader(r.Body, (64<<10)+1))
		if err != nil || len(body) > 64<<10 {
			http.Error(w, "Verification result too large.", http.StatusRequestEntityTooLarge)
			return
		}
		var callback struct {
			Param string `json:"param"`
			Error string `json:"error"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&callback) != nil || decoder.Decode(new(any)) != io.EOF {
			http.Error(w, "Invalid result.", http.StatusBadRequest)
			return
		}
		result := verificationResult{proof: VerificationProof{Param: callback.Param, Region: config.Region}}
		if callback.Error != "" {
			if callback.Param != "" || callback.Error != "failed" && callback.Error != "cancelled" {
				http.Error(w, "Invalid result.", http.StatusBadRequest)
				return
			}
			result = verificationResult{err: &VerificationError{Code: callback.Error}}
		} else if !result.proof.valid() {
			http.Error(w, "Invalid result.", http.StatusBadRequest)
			return
		}
		if !finish(result) {
			http.Error(w, "Verification already completed.", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"received":true}`))
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recover() != nil {
				finish(verificationResult{err: &VerificationError{Code: "server_failed"}})
			}
		}()
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			finish(verificationResult{err: &VerificationError{Code: "server_failed"}})
		}
	}()
	defer func() {
		shutdown, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
		<-done
		if recover() != nil {
			proof, err = VerificationProof{}, &VerificationError{Code: "open_failed"}
		}
	}()
	if err := openURL(ctx, origin+pagePath); err != nil {
		if ctx.Err() != nil {
			return VerificationProof{}, ctx.Err()
		}
		return VerificationProof{}, &VerificationError{Code: "open_failed"}
	}
	select {
	case <-ctx.Done():
		return VerificationProof{}, ctx.Err()
	case result := <-results:
		if ctx.Err() != nil {
			return VerificationProof{}, ctx.Err()
		}
		return result.proof, result.err
	}
}

func renderVerificationPage(config VerificationConfig, state string) ([]byte, error) {
	public, err := json.Marshal(struct {
		Region  string `json:"region"`
		Prefix  string `json:"prefix"`
		SceneID string `json:"sceneId"`
	}{config.Region, config.Prefix, config.SceneID})
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	err = verificationPage.Execute(&body, struct {
		State, SDK string
		Config     template.JS
	}{state, verificationSDKURL, template.JS(public)}) // validated public ASCII fields, JSON escaped
	return body.Bytes(), err
}

var verificationPage = template.Must(template.New("verification").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Z.ai verification for CQ</title>
<style>body{font:17px system-ui;max-width:40rem;margin:12vh auto;padding:1.5rem}button{font:inherit;padding:.7rem 1rem;margin-right:.5rem}#status{min-height:3rem}</style>
<h1>Z.ai verification</h1>
<p>Complete verification to return to CQ. If a challenge appears, complete it yourself. CQ will handle the selected plan after verification.</p>
<p id="status" role="status">Click Verify when you are ready.</p>
<button id="verify" type="button">Verify</button><button id="cancel" type="button">Cancel</button>
<div id="captcha"></div>
<script nonce="{{.State}}">
const config={{.Config}}, state={{.State}}, sdkURL={{.SDK}};
let settled=false;
const status=document.getElementById('status'), button=document.getElementById('verify');
async function finish(result){
  if(settled)return; settled=true; button.disabled=true;
  try {
    const r=await fetch('/result',{method:'POST',credentials:'omit',headers:{'Content-Type':'application/json','X-CQ-Verification-State':state},body:JSON.stringify(result)});
    status.textContent=r.ok?'Return to CQ. You may close this page.':'Verification could not be returned. Return to CQ and try again.';
  } catch { status.textContent='CQ is no longer waiting. You may close this page.'; }
}
document.getElementById('cancel').addEventListener('click',()=>finish({error:'cancelled'}));
function sdkFail(result){
  const code=result?.verifyCode??result?.VerifyCode, param=result?.captchaVerifyParam??result?.CaptchaVerifyParam;
  const passed=(result?.success===true&&result?.verifyResult===true)||code==='T006';
  if(passed&&typeof param==='string'&&param.trim()){finish({param});return;}
  if(passed||(result?.success===true&&result?.verifyResult===false&&(!code||code==='F015'))){status.textContent='Complete any challenge that appears.';return;}
  finish({error:'failed'});
}
button.addEventListener('click',()=>{
  if(settled||button.disabled)return; button.disabled=true; status.textContent='Loading verification…';
  window.AliyunCaptchaConfig={region:config.region,prefix:config.prefix};
  const script=document.createElement('script'); script.src=sdkURL; script.async=true;
  script.onerror=()=>finish({error:'failed'});
  script.onload=()=>{
    try {
      const pending=window.initAliyunCaptcha({SceneId:config.sceneId,region:config.region,prefix:config.prefix,mode:'popup',language:'en',element:'#captcha',button:'#verify',
        success:param=>typeof param==='string'&&param.trim()?finish({param}):finish({error:'failed'}),
        fail:sdkFail,onError:()=>finish({error:'failed'}),
        getInstance:instance=>{
          if(settled)return; status.textContent='Complete any challenge that appears.';
          try {if(typeof instance.startTracelessVerification==='function')instance.startTracelessVerification();else if(typeof instance.show==='function')instance.show();else finish({error:'failed'});}catch{finish({error:'failed'});}
        }});
      if(pending&&typeof pending.catch==='function')pending.catch(()=>finish({error:'failed'}));
    }catch{finish({error:'failed'});}
  };
  document.head.appendChild(script);
});
setTimeout(()=>finish({error:'failed'}),120000);
</script></html>`))
