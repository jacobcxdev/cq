package dots

import (
	"regexp"
	"strings"
	"testing"
)

func rspackFixture() map[string]string {
	return map[string]string{
		"index.html":          `<script type="module" src="/assets/12345.abcdef0123.js"></script>`,
		"12345.abcdef0123.js": `var e,t,n,u={bus(e,t,n){class c{static getInstance(){return this.instance}dispatchMessage(e,t){host.postMessage(t)}subscribe(){}deliverMessage(){}}let b=c.getInstance();n.d(t,{},{Bus:b})}};u.u=e=>"assets/async/"+e+"."+({67890:"abcdef0123"})[e]+".js";export{u as __webpack_require__};`,
		"async/67890.abcdef0123.js": `export const __rspack_esm_ids=[67890];export const __webpack_modules__={
 account(e,t,n){let state=atom(),account=(0,r.un)(scope.u,({get:e})=>e(state).authenticatedAccountId),user=(0,r.un)(scope.u,({get:e})=>e(state).userId??null);n.d(t,{},{State:state,Account:account,User:user})},
 facade(e,t,n){var c=n("account");let ready=(0,r.un)(scope.u,({get:e})=>{let t=e(c.State);return!t.authLoading&&("chatgpt"!==t.authMethod||null!=t.authenticatedAccountId&&null!=t.userId)});n.d(t,{chatGptAuthenticatedAccountId$:()=>c.Account,chatGptAuthenticatedUserId$:()=>c.User,Ready:()=>ready})},
 api(e,t,n){class API{async safeGet(){}async safePost(){}async safePatch(){}}let api=new API({getAdditionalHeaders:h.f,getRequestSignal:s.f});n.d(t,{},{Request:api})},
 cache(e,t,n){var i=n("facade");class C{readRoom(){}readHistory(){}read(e,t,n,r,i,a){return this.abort.signal}}function cache(e){let t=e.get(store,void 0),n=e.get(i.chatGptAuthenticatedAccountId$),r=e.get(i.chatGptAuthenticatedUserId$);if(t.cache?.accountId!==n||t.cache.userId!==r||t.cache.disposed){let c=new C(n,r,()=>true);e.watch(f);t.stopWatching=f}return t.cache}n.d(t,{Cache:()=>cache})},
 executor(e,t,n){function executor(){return services.appServices?.tppLocalExecutor}n.d(t,{},{Executor:executor})},
 profile(e,t,n){var auth=n("facade"),api=n("api");let profile=(0,r.vU)(scope.u,(e,{get:t,scope:n})=>{let r=t(auth.chatGptAuthenticatedAccountId$),a=t(auth.chatGptAuthenticatedUserId$);return{queryKey:["cloud-aeon",r,a,e],queryFn:async()=>{signal.throwIfAborted();if(n.get(auth.chatGptAuthenticatedAccountId$)===r&&n.get(auth.chatGptAuthenticatedUserId$)===a)return api.Request.safeGet("/tbo/{tbo_id}",{tbo_id:e,additionalHeaders:{[headers.owner]:r}})}}});n.d(t,{},{Profile:profile})},
 recovery(e,t,n){var reader=n("reader"),auth=n("facade");function key(e){let t=e(auth.chatGptAuthenticatedAccountId$),n=e(auth.chatGptAuthenticatedUserId$);return ` + "`cloud-aeon-profiles-v1:${JSON.stringify([t,n])}`" + `}function snapshot(e){let t=key(e.get);return null==t?null:{key:t,entries:(0,reader.Read)(t,{})}}n.d(t,{},{Snapshot:snapshot})},
 reader(e,t,n){function E(){throw Error("Persisted atom store accessed before initialization")}n.d(t,{},{Read:function(e,t){return"u"<typeof window?t:(E(),store.has(e)?store.get(e):t)}})}
 };`,
	}
}
func TestNativeRspackDiscoveredSemanticExports(t *testing.T) {
	assets := rspackFixture()
	runtime, b, err := resolveNativeRspack(assets)
	if err != nil {
		t.Fatal(err)
	}
	if runtime != "12345.abcdef0123.js" || b.Rspack.Account.Module != "account" || b.Rspack.Account.Export != "Account" || b.Rspack.Bus.Export != "Bus" {
		t.Fatalf("unexpected bindings: %#v", b)
	}
	// Local parameter renaming must not turn a harmless minifier rebuild into a version gate.
	changedLocals := map[string]string{}
	locals := regexp.MustCompile(`\b(e|t|n|r|a)\b`)
	for name, source := range assets {
		changedLocals[name] = locals.ReplaceAllStringFunc(source, func(value string) string { return value + "Next" })
	}
	if _, _, err := resolveNativeRspack(changedLocals); err != nil {
		t.Fatalf("renamed local parameters: %v", err)
	}
	for _, hash := range []string{"a", "abcdef0123456789abcdef"} {
		changed := map[string]string{}
		for name, source := range assets {
			changed[strings.ReplaceAll(name, "abcdef0123", hash)] = strings.ReplaceAll(source, "abcdef0123", hash)
		}
		if _, _, err := resolveNativeRspack(changed); err != nil {
			t.Fatalf("hash length %d: %v", len(hash), err)
		}
	}
	// Module IDs and export identifiers are discovered independently of build IDs.
	renamed := map[string]string{}
	replacements := strings.NewReplacer(`account(e,t,n)`, `aNew(e,t,n)`, `n("account")`, `n("aNew")`, `Account:account`, `Anew:account`, `.Account`, `.Anew`, `chatGptAuthenticatedAnewId$`, `chatGptAuthenticatedAccountId$`)
	for k, v := range assets {
		renamed[k] = replacements.Replace(v)
	}
	if _, _, err := resolveNativeRspack(renamed); err != nil {
		t.Fatal(err)
	}
}
func TestNativeRspackRejectedContractDrift(t *testing.T) {
	for _, variant := range []string{"API", "cache", "executor", "account", "ready", "recovery", "entry", "ambiguous", "wrong API class", "wrong profile API", "wrong recovery owner", "mixed ready state", "foreign chunk"} {
		t.Run(variant, func(t *testing.T) {
			assets := rspackFixture()
			core := "async/67890.abcdef0123.js"
			switch variant {
			case "API":
				assets[core] = strings.ReplaceAll(assets[core], "async safePatch()", "async wrongPatch()")
			case "cache":
				assets[core] = strings.ReplaceAll(assets[core], "read(e,t,n,r,i,a)", "read(e,t,n,r,i)")
			case "executor":
				assets[core] = strings.ReplaceAll(assets[core], "?.tppLocalExecutor}", "?.otherExecutor}")
			case "account":
				assets[core] = strings.ReplaceAll(assets[core], ".authenticatedAccountId)", ".otherAccountId)")
			case "ready":
				assets[core] = strings.ReplaceAll(assets[core], "!t.authLoading&&", "t.authLoading&&")
			case "recovery":
				assets[core] = strings.ReplaceAll(assets[core], "JSON.stringify([t,n])", "JSON.stringify([n,t])")
			case "wrong API class":
				assets[core] = strings.ReplaceAll(assets[core], "new API(", "new Untrusted(")
			case "wrong profile API":
				assets[core] = strings.ReplaceAll(assets[core], "api.Request.safeGet", "auth.Ready.safeGet")
			case "wrong recovery owner":
				assets[core] = strings.ReplaceAll(assets[core], "function key(e){let t=e(auth.chatGptAuthenticatedAccountId$)", "function key(e){let t=e(reader.Read)")
			case "mixed ready state":
				assets[core] = strings.ReplaceAll(assets[core], "!t.authLoading&&", "!other.authLoading&&")
			case "foreign chunk":
				assets["12345.abcdef0123.js"] = strings.ReplaceAll(assets["12345.abcdef0123.js"], `67890:"abcdef0123"`, `67890:"bbbbbbbbbb"`)
			case "entry":
				assets["index.html"] = ""
			case "ambiguous":
				assets["async/78901.abcdef0123.js"] = strings.ReplaceAll(assets[core], "account(e,t,n)", "other(e,t,n)")
			}
			if _, _, err := resolveNativeRspack(assets); err == nil {
				t.Fatal("changed contract accepted")
			}
		})
	}
}
func TestNativeRspackModuleScanExcludedNestedMethods(t *testing.T) {
	assets := map[string]string{"12345.abcdef0123.js": `var e,t,n,u={actual(e,t,n){let x={fake(e,t,n){return /[{}]/.test(` + "`nested${{x:`inner${1}`}}`" + `)}};n.d(t,{},{account:f(x.authenticatedAccountId)})}};u.u=e=>"assets/async/"+e+"."+({67890:"abcdef0123"})[e]+".js";export{u as __webpack_require__};`}
	modules, err := rspackModules(assets)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := modules["fake"]; ok {
		t.Fatal("nested method became module")
	}
}
