package dots

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// NativeRspackExport names an export in an already registered renderer module.
type NativeRspackExport struct {
	Module string `json:"module"`
	Export string `json:"export"`
}
type NativeRspackBindings struct {
	API             NativeRspackExport `json:"api"`
	Account         NativeRspackExport `json:"account"`
	User            NativeRspackExport `json:"user"`
	Ready           NativeRspackExport `json:"ready"`
	Cache           NativeRspackExport `json:"cache"`
	Executor        NativeRspackExport `json:"executor"`
	ProfileQuery    NativeRspackExport `json:"profile_query"`
	ProfileRecovery NativeRspackExport `json:"profile_recovery"`
	Bus             NativeRspackExport `json:"bus"`
}

func (b *NativeRspackBindings) validate() error {
	if b == nil {
		return errors.New("native Rspack bindings unavailable")
	}
	for _, v := range []NativeRspackExport{b.API, b.Account, b.User, b.Ready, b.Cache, b.Executor, b.ProfileQuery, b.ProfileRecovery, b.Bus} {
		if !regexp.MustCompile(`^[A-Za-z0-9_$]{1,128}$`).MatchString(v.Module) || !jsIdentifierPattern.MatchString(v.Export) {
			return errors.New("invalid native Rspack reference")
		}
	}
	return nil
}

type rspackModule struct{ id, source, require, exports string }

func rspackModules(assets map[string]string) (map[string]rspackModule, error) {
	result := map[string]rspackModule{}
	tablePattern := regexp.MustCompile(`(?:__webpack_modules__=|(?:^|;)var[^{}]{0,256},` + jsIdentifier + `=)\{`)
	header := regexp.MustCompile(`^([A-Za-z0-9_$]+)\((?:(` + jsIdentifier + `)(?:,(` + jsIdentifier + `))?(?:,(` + jsIdentifier + `))?)?\)\{`)
	for asset, source := range assets {
		core := strings.Contains(source, ".cache?.accountId") && strings.Contains(source, "cloud-aeon-profiles-v1:") && strings.Contains(source, ".appServices?.tppLocalExecutor}")
		if !core && !strings.Contains(source, "as __webpack_require__}") {
			continue
		}
		table := tablePattern.FindStringIndex(source)
		if table == nil {
			continue
		}
		offset := table[1]
		for offset < len(source) && source[offset] != '}' {
			for offset < len(source) && strings.ContainsRune(" \n\r\t", rune(source[offset])) {
				offset++
			}
			if offset >= len(source) || source[offset] == '}' {
				break
			}
			match := header.FindStringSubmatchIndex(source[offset:])
			if match == nil {
				return nil, fmt.Errorf("native Rspack table entry invalid: %s", asset)
			}
			brace := offset + match[1] - 1
			body, ok := rspackBody(source, brace)
			if !ok {
				return nil, errors.New("native Rspack module body invalid")
			}
			id := source[offset+match[2] : offset+match[3]]
			if match[8] >= 0 {
				module := rspackModule{id: id, source: source[offset:brace] + body, require: source[offset+match[8] : offset+match[9]], exports: source[offset+match[6] : offset+match[7]]}
				if !rspackRelevant(module.source) {
					offset = brace + len(body)
					if offset < len(source) && source[offset] == ',' {
						offset++
					}
					continue
				}
				if previous, exists := result[id]; exists && previous.source != module.source {
					return nil, fmt.Errorf("native Rspack module identity ambiguous: %s", id)
				}
				result[id] = module
			}
			offset = brace + len(body)
			if offset < len(source) && source[offset] == ',' {
				offset++
			}
		}
	}
	if len(result) == 0 {
		return nil, errors.New("native Rspack modules unavailable")
	}
	return result, nil
}
func rspackTables(m rspackModule) []string {
	needle := m.require + ".d(" + m.exports + ","
	var result []string
	for offset := 0; offset < len(m.source); {
		i := strings.Index(m.source[offset:], needle)
		if i < 0 {
			break
		}
		i += offset + len(needle)
		for i < len(m.source) && m.source[i] == '{' {
			body, ok := rspackBody(m.source, i)
			if !ok {
				break
			}
			result = append(result, body)
			i += len(body)
			if i >= len(m.source) || m.source[i] != ',' {
				break
			}
			i++
		}
		offset = i
	}
	return result
}
func rspackExport(m rspackModule, local string) (NativeRspackExport, error) {
	pattern := regexp.MustCompile(`(?:^|[,{])(` + jsIdentifier + `):(?:\(\)=>)?` + regexp.QuoteMeta(local) + `(?:[,}])`)
	found := map[string]bool{}
	for _, table := range rspackTables(m) {
		for _, v := range pattern.FindAllStringSubmatch(table, -1) {
			found[v[1]] = true
		}
	}
	if len(found) == 0 {
		return NativeRspackExport{}, errors.New("native Rspack export unavailable")
	}
	names := make([]string, 0, len(found))
	for k := range found {
		names = append(names, k)
	}
	sort.Strings(names)
	// Aliases of the same local are equivalent. Prefer the stable named export.
	for _, name := range names {
		if len(name) > 3 {
			return NativeRspackExport{m.id, name}, nil
		}
	}
	return NativeRspackExport{m.id, names[0]}, nil
}
func rspackMatch(modules map[string]rspackModule, pattern string, guard func(string) bool) (rspackModule, []string, error) {
	re := regexp.MustCompile(pattern)
	var owner rspackModule
	var match []string
	for _, m := range modules {
		if guard != nil && !guard(m.source) {
			continue
		}
		for _, v := range re.FindAllStringSubmatch(m.source, -1) {
			if match != nil {
				return rspackModule{}, nil, errors.New("native Rspack contract ambiguous")
			}
			owner, match = m, v
		}
	}
	if match == nil {
		return owner, nil, errors.New("native Rspack contract unavailable")
	}
	return owner, match, nil
}
func rspackImport(m rspackModule, alias string) (string, error) {
	pattern := regexp.MustCompile(`(?:^|[ ,;])` + regexp.QuoteMeta(alias) + `=` + regexp.QuoteMeta(m.require) + `\("([A-Za-z0-9_$]+)"\)`)
	matches := pattern.FindAllStringSubmatch(m.source, -1)
	if len(matches) != 1 {
		return "", errors.New("native Rspack dependency ambiguous")
	}
	return matches[0][1], nil
}
func resolveNativeRspack(assets map[string]string) (string, *NativeBindings, error) {
	runtime := ""
	document := assets["index.html"]
	if document == "" {
		return "", nil, errors.New("native Rspack entry document unavailable")
	}
	selected := map[string]string{}
	for name, source := range assets {
		if strings.Contains(source, ".cache?.accountId") && strings.Contains(source, "cloud-aeon-profiles-v1:") && strings.Contains(source, ".appServices?.tppLocalExecutor}") {
			selected[name] = source
		}
		if regexp.MustCompile(`export\{`+jsIdentifier+` as __webpack_require__\}`).MatchString(source) && strings.Contains(document, `src="/assets/`+name+`"`) {
			if runtime != "" {
				return "", nil, errors.New("native Rspack runtime ambiguous")
			}
			runtime = name
			selected[name] = source
		}
	}
	if runtime == "" {
		return "", nil, errors.New("native Rspack runtime unavailable")
	}
	cores := 0
	for name, source := range selected {
		if name == runtime {
			continue
		}
		cores++
		if err := rspackChunkReference(assets[runtime], name, source); err != nil {
			return "", nil, err
		}
	}
	if cores != 1 {
		return "", nil, errors.New("native Rspack core chunk missing or ambiguous")
	}
	modules, err := rspackModules(selected)
	if err != nil {
		return "", nil, err
	}
	b := &NativeRspackBindings{}
	fail := func(contract string, err error) (string, *NativeBindings, error) {
		return "", nil, fmt.Errorf("native Rspack %s: %w", contract, err)
	}
	id := jsIdentifier
	account, a, err := rspackMatch(modules, `(`+id+`)=\(0,`+id+`\.`+id+`\)\(`+id+`\.`+id+`,\(\{get:(`+id+`)\}\)=>(`+id+`)\((`+id+`)\)\.authenticatedAccountId\)`, nil)
	if err != nil || a[2] != a[3] {
		return fail("account", errors.New("account atom changed"))
	}
	b.Account, err = rspackExport(account, a[1])
	if err != nil {
		return fail("account export", err)
	}
	user := regexp.MustCompile(`(`+id+`)=\(0,`+id+`\.`+id+`\)\(`+id+`\.`+id+`,\(\{get:(`+id+`)\}\)=>(`+id+`)\(`+regexp.QuoteMeta(a[4])+`\)\.userId\?\?null\)`).FindAllStringSubmatch(account.source, -1)
	if len(user) != 1 || user[0][2] != user[0][3] {
		return fail("user", errors.New("user atom changed"))
	}
	b.User, err = rspackExport(account, user[0][1])
	if err != nil {
		return fail("user export", err)
	}
	ready, r, err := rspackMatch(modules, `(`+id+`)=\(0,`+id+`\.`+id+`\)\(`+id+`\.`+id+`,\(\{get:(`+id+`)\}\)=>\{let (`+id+`)=(`+id+`)\((`+id+`)\.(`+id+`)\);return!`+id+`\.authLoading&&\("chatgpt"!==`+id+`\.authMethod\|\|null!=`+id+`\.authenticatedAccountId&&null!=`+id+`\.userId\)\}\)`, nil)
	if err != nil {
		return fail("ready", err)
	}
	dependency, err := rspackImport(ready, r[5])
	if err != nil || dependency != account.id {
		return fail("ready identity", errors.New("readiness account owner changed"))
	}
	state, err := rspackExport(account, a[4])
	if err != nil || state.Export != r[6] || r[2] != r[4] {
		return fail("ready identity", errors.New("readiness state changed"))
	}
	if !strings.Contains(r[0], "return!"+r[3]+`.authLoading&&("chatgpt"!==`+r[3]+".authMethod||null!="+r[3]+".authenticatedAccountId&&null!="+r[3]+".userId)") {
		return fail("ready identity", errors.New("readiness value changed"))
	}
	b.Ready, err = rspackExport(ready, r[1])
	if err != nil {
		return fail("ready export", err)
	}
	api, v, err := rspackMatch(modules, `(?:let|var|const) (`+id+`)=new (`+id+`)\(\{getAdditionalHeaders:`+id+`\.`+id+`,getRequestSignal:`+id+`\.`+id+`\}\)`, func(s string) bool {
		return strings.Contains(s, "async safeGet(") && strings.Contains(s, "async safePost(") && strings.Contains(s, "async safePatch(")
	})
	if err != nil {
		return fail("API", err)
	}
	apiBody, classErr := rspackDeclarationBody(api.source, `class `+regexp.QuoteMeta(v[2])+`\{`)
	if classErr != nil || !strings.Contains(apiBody, "async safeGet(") || !strings.Contains(apiBody, "async safePost(") || !strings.Contains(apiBody, "async safePatch(") {
		return fail("API class", errors.New("authenticated API methods changed"))
	}
	b.API, err = rspackExport(api, v[1])
	if err != nil {
		return fail("API export", err)
	}
	cache, v, err := rspackMatch(modules, `function (`+id+`)\((`+id+`)\)\{let (`+id+`)=(`+id+`)\.get\(`+id+`,void 0\),(`+id+`)=(`+id+`)\.get\((`+id+`)\.(`+id+`)\),(`+id+`)=(`+id+`)\.get\((`+id+`)\.(`+id+`)\);`, func(s string) bool { return strings.Contains(s, ".cache?.accountId") })
	if err != nil {
		return fail("cache", err)
	}
	if v[2] != v[4] || v[2] != v[6] || v[2] != v[10] || v[7] != v[11] {
		return fail("cache identity", errors.New("cache scope changed"))
	}
	if err = validateRspackAccountDependency(modules, cache, v[7], v[8], v[12], b); err != nil {
		return fail("cache identity", err)
	}
	factoryBody, err := rspackDeclarationBody(cache.source, `function `+regexp.QuoteMeta(v[1])+`\(`+regexp.QuoteMeta(v[2])+`\)\{`)
	if err != nil {
		return fail("cache factory", err)
	}
	for _, needle := range []string{v[3] + ".cache?.accountId!==" + v[5], v[3] + ".cache.userId!==" + v[9], v[3] + ".cache.disposed", v[2] + ".watch(", v[3] + ".stopWatching="} {
		if !strings.Contains(factoryBody, needle) {
			return fail("cache ownership", errors.New("cache ownership changed"))
		}
	}
	constructors := regexp.MustCompile(`new (`+id+`)\(`+regexp.QuoteMeta(v[5])+`,`+regexp.QuoteMeta(v[9])+`,`).FindAllStringSubmatch(factoryBody, -1)
	if len(constructors) != 1 {
		return fail("cache constructor", errors.New("cache constructor changed"))
	}
	cacheBody, err := rspackDeclarationBody(cache.source, `class `+regexp.QuoteMeta(constructors[0][1])+`\{`)
	if err != nil || !strings.Contains(cacheBody, "readRoom(") || !strings.Contains(cacheBody, "readHistory(") || !strings.Contains(cacheBody, "this.abort.signal") || !regexp.MustCompile(`read\(`+id+`,`+id+`,`+id+`,`+id+`,`+id+`,`+id+`\)\{`).MatchString(cacheBody) {
		return fail("cache reads", errors.New("owned six-argument cache read changed"))
	}
	b.Cache, err = rspackExport(cache, v[1])
	if err != nil {
		return fail("cache export", err)
	}
	executor, v, err := rspackMatch(modules, `function (`+id+`)\(\)\{return `+id+`\.appServices\?\.tppLocalExecutor\}`, nil)
	if err != nil {
		return fail("executor", err)
	}
	b.Executor, err = rspackExport(executor, v[1])
	if err != nil {
		return fail("executor export", err)
	}
	profile, v, err := rspackMatch(modules, `(?:let|var|const) (`+id+`)=\(0,`+id+`\.`+id+`\)\(`+id+`\.`+id+`,\((`+id+`),\{get:(`+id+`),scope:(`+id+`)\}\)=>\{let (`+id+`)=(`+id+`)\((`+id+`)\.(`+id+`)\),(`+id+`)=(`+id+`)\((`+id+`)\.(`+id+`)\)`, func(s string) bool { return strings.Contains(s, `queryKey:["cloud-aeon",`) })
	if err != nil {
		return fail("profile", err)
	}
	if v[3] != v[6] || v[3] != v[10] || v[7] != v[11] {
		return fail("profile identity", errors.New("query scope changed"))
	}
	if err = validateRspackAccountDependency(modules, profile, v[7], v[8], v[12], b); err != nil {
		return fail("profile identity", err)
	}
	start := strings.Index(profile.source, v[0])
	brace := start + strings.LastIndex(v[0], "=>{") + 2
	profileBody, ok := rspackBody(profile.source, brace)
	if !ok || !strings.Contains(profileBody, `queryKey:["cloud-aeon",`+v[5]+`,`+v[9]+`,`+v[2]+`]`) || !strings.Contains(profileBody, `safeGet("/tbo/{tbo_id}"`) || !strings.Contains(profileBody, "tbo_id:"+v[2]) || !strings.Contains(profileBody, ".throwIfAborted()") {
		return fail("profile query", errors.New("profile ownership changed"))
	}
	requestRefs := regexp.MustCompile(`(`+id+`)\.(`+id+`)\.safeGet\("/tbo/\{tbo_id\}"`).FindAllStringSubmatch(profileBody, -1)
	if len(requestRefs) != 1 {
		return fail("profile API", errors.New("profile API owner changed"))
	}
	requestModule, err := rspackImport(profile, requestRefs[0][1])
	if err != nil {
		return fail("profile API", err)
	}
	requestOwner, requestErr := rspackExportOwner(modules, NativeRspackExport{requestModule, requestRefs[0][2]}, 0)
	apiOwner, apiErr := rspackExportOwner(modules, b.API, 0)
	if requestErr != nil || apiErr != nil || requestOwner != apiOwner {
		return fail("profile API", errors.New("profile API owner changed"))
	}
	headers := regexp.MustCompile(`additionalHeaders:\{\[` + id + `\.` + id + `\]:` + regexp.QuoteMeta(v[5]) + `\}`).MatchString(profileBody)
	if !headers || !strings.Contains(profileBody, v[4]+".get("+v[7]+"."+v[8]+")==="+v[5]) || !strings.Contains(profileBody, v[4]+".get("+v[7]+"."+v[12]+")==="+v[9]) {
		return fail("profile ownership", errors.New("profile request identity changed"))
	}
	b.ProfileQuery, err = rspackExport(profile, v[1])
	if err != nil {
		return fail("profile export", err)
	}
	recovery, key, err := rspackMatch(modules, `function (`+id+`)\((`+id+`)\)\{let (`+id+`)=(`+id+`)\((`+id+`)\.(`+id+`)\),(`+id+`)=(`+id+`)\((`+id+`)\.(`+id+`)\);return`, func(s string) bool { return strings.Contains(s, "cloud-aeon-profiles-v1:") })
	if err != nil {
		return fail("profile recovery", err)
	}
	if key[2] != key[4] || key[2] != key[8] || key[5] != key[9] {
		return fail("recovery identity", errors.New("recovery identity scope changed"))
	}
	if err = validateRspackAccountDependency(modules, recovery, key[5], key[6], key[10], b); err != nil {
		return fail("recovery identity", err)
	}
	keyBody, err := rspackDeclarationBody(recovery.source, `function `+regexp.QuoteMeta(key[1])+`\(`+regexp.QuoteMeta(key[2])+`\)\{`)
	if err != nil || !strings.Contains(keyBody, "cloud-aeon-profiles-v1:${JSON.stringify(["+key[3]+","+key[7]+"])}") {
		return fail("recovery key", errors.New("recovery key changed"))
	}
	snapshotPattern := regexp.MustCompile(`function ` + id + `\((` + id + `)\)\{let (` + id + `)=` + regexp.QuoteMeta(key[1]) + `\((` + id + `)\.get\);return null==(` + id + `)\?null:\{key:(` + id + `),entries:\(0,(` + id + `)\.(` + id + `)\)\((` + id + `),\{\}\)\}\}`)
	snapshots := snapshotPattern.FindAllStringSubmatch(recovery.source, -1)
	if len(snapshots) != 1 {
		return fail("recovery snapshot", errors.New("snapshot changed"))
	}
	snapshot := snapshots[0]
	if snapshot[1] != snapshot[3] || snapshot[2] != snapshot[4] || snapshot[2] != snapshot[5] || snapshot[2] != snapshot[8] {
		return fail("recovery snapshot", errors.New("snapshot key changed"))
	}
	reader, err := rspackImport(recovery, snapshot[6])
	if err != nil {
		return fail("recovery reader", err)
	}
	readerModule, ok := modules[reader]
	if !ok || !strings.Contains(readerModule.source, "Persisted atom store accessed before initialization") {
		return fail("recovery reader", errors.New("reader ownership changed"))
	}
	readerPattern := regexp.MustCompile(regexp.QuoteMeta(snapshot[7]) + `:function\((` + id + `),(` + id + `)\)\{return"u"<typeof window\?(` + id + `):\(` + id + `\(\),(` + id + `)\.has\((` + id + `)\)\?(` + id + `)\.get\((` + id + `)\):(` + id + `)\)\}`)
	readers := readerPattern.FindAllStringSubmatch(strings.Join(rspackTables(readerModule), ";"), -1)
	if len(readers) != 1 {
		return fail("recovery reader", errors.New("reader contract changed"))
	}
	read := readers[0]
	if read[1] != read[5] || read[1] != read[7] || read[2] != read[3] || read[2] != read[8] || read[4] != read[6] {
		return fail("recovery reader", errors.New("reader key changed"))
	}
	b.ProfileRecovery = NativeRspackExport{reader, snapshot[7]}
	bus, v, err := rspackMatch(modules, `(?:let|var|const) (`+id+`)=(`+id+`)\.getInstance\(\)`, func(s string) bool {
		return strings.Contains(s, "dispatchMessage(") && strings.Contains(s, "subscribe(") && strings.Contains(s, "deliverMessage(") && strings.Contains(s, ".postMessage(") && strings.Contains(s, "static getInstance()")
	})
	if err != nil {
		return fail("message bus", err)
	}
	busBody, classErr := rspackDeclarationBody(bus.source, `class `+regexp.QuoteMeta(v[2])+`\{`)
	for _, method := range []string{"static getInstance()", "dispatchMessage(", "subscribe(", "deliverMessage(", ".postMessage("} {
		if classErr != nil || !strings.Contains(busBody, method) {
			return fail("message bus class", errors.New("bus singleton methods changed"))
		}
	}
	b.Bus, err = rspackExport(bus, v[1])
	if err != nil {
		return fail("message bus export", err)
	}
	if err := b.validate(); err != nil {
		return fail("references", err)
	}
	return runtime, &NativeBindings{API: "Ymr", Account: "GMr", User: "XMr", Ready: "JMr", Cache: "Lpr", Executor: "VAn", ProfileQuery: "CQProfileQuery", ProfileRecovery: "CQProfileRecovery", Bus: "CQBus", BusAsset: runtime, Rspack: b}, nil
}
func validateRspackAccountDependency(modules map[string]rspackModule, m rspackModule, alias, account, user string, b *NativeRspackBindings) error {
	id, err := rspackImport(m, alias)
	if err != nil {
		return err
	}
	target, ok := modules[id]
	if !ok {
		return errors.New("account dependency unavailable")
	}
	for _, pair := range [][2]string{{account, b.Account.Export}, {user, b.User.Export}} {
		if id == b.Account.Module && pair[0] == pair[1] {
			continue
		}
		// The account facade re-exports the same authenticated atoms.
		pattern := regexp.MustCompile(regexp.QuoteMeta(pair[0]) + `:\(\)=>(` + jsIdentifier + `)\.` + regexp.QuoteMeta(pair[1]) + `(?:[,}])`)
		matches := pattern.FindAllStringSubmatch(target.source, -1)
		if len(matches) != 1 {
			return errors.New("account atom re-export changed")
		}
		source, err := rspackImport(target, matches[0][1])
		if err != nil || source != b.Account.Module {
			return errors.New("account atom owner changed")
		}
	}
	return nil
}

func rspackRelevant(source string) bool {
	for _, needle := range []string{".authenticatedAccountId)", "chatGptAuthenticatedAccountId$:()=>", "Request:()=>", ".cache?.accountId", "cloud-aeon-profiles-v1:", "Persisted atom store accessed before initialization", `queryKey:["cloud-aeon",`} {
		if strings.Contains(source, needle) {
			return true
		}
	}
	return strings.Contains(source, "getAdditionalHeaders:") && strings.Contains(source, "getRequestSignal:") || strings.Contains(source, "return ") && strings.Contains(source, ".appServices?.tppLocalExecutor}") || strings.Contains(source, "dispatchMessage(") && strings.Contains(source, "deliverMessage(") && strings.Contains(source, "static getInstance()")
}

func rspackDeclarationBody(source, prefix string) (string, error) {
	matches := regexp.MustCompile(prefix).FindAllStringIndex(source, -1)
	if len(matches) != 1 {
		return "", errors.New("native Rspack declaration ambiguous")
	}
	brace := matches[0][1] - 1
	body, ok := rspackBody(source, brace)
	if !ok {
		return "", errors.New("native Rspack declaration body invalid")
	}
	return body, nil
}

func rspackChunkReference(runtime, name, source string) error {
	parts := regexp.MustCompile(`^async/([0-9]+)\.([a-f0-9]{1,64})\.js$`).FindStringSubmatch(name)
	if parts == nil {
		return errors.New("native Rspack core filename invalid")
	}
	ids := regexp.MustCompile(`__rspack_esm_ids=\[([0-9]+)\]`).FindAllStringSubmatch(source, -1)
	if len(ids) != 1 || ids[0][1] != parts[1] {
		return errors.New("native Rspack chunk identity changed")
	}
	loader := regexp.MustCompile(`\.u=(`+jsIdentifier+`)=>"assets/async/"\+(`+jsIdentifier+`)\+"\."\+\(\{`).FindAllStringSubmatchIndex(runtime, -1)
	if len(loader) != 1 {
		return errors.New("native Rspack chunk loader changed")
	}
	m := loader[0]
	if runtime[m[2]:m[3]] != runtime[m[4]:m[5]] {
		return errors.New("native Rspack chunk loader identity changed")
	}
	body, ok := rspackBody(runtime, m[1]-1)
	if !ok {
		return errors.New("native Rspack chunk map invalid")
	}
	if !strings.HasPrefix(runtime[m[1]-1+len(body):], `)[`+runtime[m[2]:m[3]]+`]+".js"`) {
		return errors.New("native Rspack chunk suffix changed")
	}
	values := regexp.MustCompile(`(?:[,{])`+regexp.QuoteMeta(parts[1])+`:"([a-f0-9]{1,64})"(?:[,}])`).FindAllStringSubmatch(body, -1)
	if len(values) != 1 || values[0][1] != parts[2] {
		return errors.New("native Rspack core chunk not owned by runtime")
	}
	return nil
}

func rspackExportOwner(modules map[string]rspackModule, ref NativeRspackExport, depth int) (string, error) {
	if depth > 8 {
		return "", errors.New("native Rspack export chain too deep")
	}
	m, ok := modules[ref.Module]
	if !ok {
		return "", errors.New("native Rspack export owner unavailable")
	}
	pattern := regexp.MustCompile(`(?:[,{])` + regexp.QuoteMeta(ref.Export) + `:(?:\(\)=>)?(` + jsIdentifier + `)(?:\.(` + jsIdentifier + `))?(?:[,}])`)
	values := pattern.FindAllStringSubmatch(strings.Join(rspackTables(m), ";"), -1)
	if len(values) != 1 {
		return "", errors.New("native Rspack export owner ambiguous")
	}
	v := values[0]
	if v[2] == "" {
		return m.id + "." + v[1], nil
	}
	imported, err := rspackImport(m, v[1])
	if err != nil {
		return "", err
	}
	return rspackExportOwner(modules, NativeRspackExport{imported, v[2]}, depth+1)
}
