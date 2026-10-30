package dots

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// NativeBindings records semantic exports discovered from an application
// snapshot. Names are identifiers, never JavaScript supplied by configuration.
type NativeBindings struct {
	API      string `json:"api"`
	Account  string `json:"account"`
	User     string `json:"user"`
	Ready    string `json:"ready"`
	Cache    string `json:"cache"`
	Executor string `json:"executor"`
	BusAsset string `json:"bus_asset"`
	Bus      string `json:"bus"`
}

const jsIdentifier = `[A-Za-z_$][A-Za-z0-9_$]*`

var jsIdentifierPattern = regexp.MustCompile(`^` + jsIdentifier + `$`)

func (b NativeBindings) validate() error {
	for _, name := range []string{b.API, b.Account, b.User, b.Ready, b.Cache, b.Executor, b.Bus} {
		if !jsIdentifierPattern.MatchString(name) {
			return errors.New("invalid native renderer binding")
		}
	}
	if !modulePattern.MatchString(b.BusAsset) || !strings.HasPrefix(b.BusAsset, "app-shared-") {
		return errors.New("invalid native message bus asset")
	}
	return nil
}

type nativeArchiveEntry struct {
	Files    map[string]nativeArchiveEntry `json:"files"`
	Size     int64                         `json:"size"`
	Offset   string                        `json:"offset"`
	Unpacked bool                          `json:"unpacked"`
	Link     string                        `json:"link"`
}

// readNativeModules reads only bounded renderer modules from Electron's ASAR
// index. Nothing is extracted, executed or written into the signed bundle.
func readNativeModules(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var prefix [16]byte
	if _, err := f.ReadAt(prefix[:], 0); err != nil {
		return nil, errors.New("renderer archive header unavailable")
	}
	headerSize := int64(binary.LittleEndian.Uint32(prefix[12:]))
	dataStart := 8 + int64(binary.LittleEndian.Uint32(prefix[4:]))
	if binary.LittleEndian.Uint32(prefix[:4]) != 4 || headerSize < 2 || headerSize > 16<<20 || dataStart < 16+headerSize || dataStart > info.Size() {
		return nil, errors.New("renderer archive header invalid")
	}
	header := make([]byte, headerSize)
	if _, err := f.ReadAt(header, 16); err != nil {
		return nil, errors.New("renderer archive index unavailable")
	}
	var root nativeArchiveEntry
	if err := json.Unmarshal(header, &root); err != nil {
		return nil, errors.New("renderer archive index invalid")
	}
	assets := root.Files["webview"].Files["assets"].Files
	modules := map[string]string{}
	for name, entry := range assets {
		if !modulePattern.MatchString(name) {
			continue
		}
		if len(modules) >= 64 || entry.Unpacked || entry.Link != "" || entry.Size < 1 || entry.Size > 32<<20 {
			return nil, errors.New("renderer module bounds invalid")
		}
		offset, err := strconv.ParseInt(entry.Offset, 10, 64)
		if err != nil || offset < 0 || offset > info.Size()-dataStart || entry.Size > info.Size()-dataStart-offset {
			return nil, errors.New("renderer module offset invalid")
		}
		data := make([]byte, entry.Size)
		if _, err := f.ReadAt(data, dataStart+offset); err != nil {
			return nil, errors.New("renderer module unavailable")
		}
		modules[name] = string(data)
	}
	if len(modules) == 0 {
		return nil, errors.New("native renderer modules unavailable")
	}
	return modules, nil
}

func discoverNativeBindings(path string) (string, *NativeBindings, error) {
	modules, err := readNativeModules(path)
	if err != nil {
		return "", nil, err
	}
	return resolveNativeBindings(modules)
}

func uniqueNativeMatch(source, pattern, contract string) ([]string, error) {
	matches := regexp.MustCompile(pattern).FindAllStringSubmatch(source, -1)
	if len(matches) != 1 {
		return nil, fmt.Errorf("native %s contract missing or ambiguous", contract)
	}
	return matches[0], nil
}

func nativeExport(source, local string) (string, error) {
	index := strings.LastIndex(source, "export{")
	if index < 0 {
		index = strings.LastIndex(source, "export {")
	}
	if index < 0 {
		return "", errors.New("native renderer export table unavailable")
	}
	match, err := uniqueNativeMatch(source[index:], `(?:[,{]|\s)`+regexp.QuoteMeta(local)+`\s+as\s+(`+jsIdentifier+`)(?:[,}])`, "export")
	if err != nil {
		return "", err
	}
	return match[1], nil
}

// nativeJSBody locates a declaration's balanced body. String and comment bodies
// do not contribute braces. No source text becomes an executable adapter.
func nativeJSBody(source string, start int) (string, bool) {
	if start < 0 || start >= len(source) || source[start] != '{' {
		return "", false
	}
	depth, quote, escaped := 0, byte(0), false
	for i := start; i < len(source) && i-start < 128<<10; i++ {
		c := source[i]
		if quote != 0 {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			continue
		}
		if c == '/' && i+1 < len(source) && source[i+1] == '/' {
			if end := strings.IndexByte(source[i+2:], '\n'); end >= 0 {
				i += end + 2
				continue
			}
			return "", false
		}
		if c == '/' && i+1 < len(source) && source[i+1] == '*' {
			if end := strings.Index(source[i+2:], "*/"); end >= 0 {
				i += end + 3
				continue
			}
			return "", false
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return source[start : i+1], true
			}
		}
	}
	return "", false
}

func nativeClass(source, name string) (string, error) {
	needle := name + "=class"
	var bodies []string
	for offset := 0; offset < len(source); {
		index := strings.Index(source[offset:], needle)
		if index < 0 {
			break
		}
		index += offset
		offset = index + len(needle)
		if index > 0 && source[index-1] != ',' && source[index-1] != ';' {
			continue
		}
		brace := strings.IndexByte(source[offset:min(offset+256, len(source))], '{')
		if brace < 0 {
			continue
		}
		body, ok := nativeJSBody(source, offset+brace)
		if !ok {
			return "", errors.New("native renderer class body changed")
		}
		bodies = append(bodies, body)
	}
	if len(bodies) != 1 {
		return "", errors.New("native renderer class missing or ambiguous")
	}
	return bodies[0], nil
}

// Limit expensive identifier patterns to nearby semantic field names. Match
// offsets are deduplicated when two nearby occurrences share the same window.
func nativeSemanticMatch(source, pattern, needle, contract string) ([]string, error) {
	re := regexp.MustCompile(pattern)
	matches := map[int][]string{}
	for offset := 0; offset < len(source); {
		index := strings.Index(source[offset:], needle)
		if index < 0 {
			break
		}
		index += offset
		offset = index + len(needle)
		start, end := max(0, index-1024), min(len(source), index+1024)
		for _, location := range re.FindAllStringSubmatchIndex(source[start:end], -1) {
			values := make([]string, len(location)/2)
			for i := range values {
				if location[2*i] >= 0 {
					values[i] = source[start+location[2*i] : start+location[2*i+1]]
				}
			}
			matches[start+location[0]] = values
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("native %s contract missing or ambiguous", contract)
	}
	for _, match := range matches {
		return match, nil
	}
	return nil, errors.New("native contract unavailable")
}

func resolveNativeInitial(source string) (*NativeBindings, error) {
	id := jsIdentifier
	account, err := nativeSemanticMatch(source, `(`+id+`)=`+id+`\(`+id+`,\(\{get:(`+id+`)\}\)=>(`+id+`)\((`+id+`)\)\.authenticatedAccountId\)`, ".authenticatedAccountId)", "account atom")
	if err != nil || account[2] != account[3] {
		return nil, errors.New("native account atom contract missing or ambiguous")
	}
	user, err := nativeSemanticMatch(source, `(`+id+`)=`+id+`\(`+id+`,\(\{get:(`+id+`)\}\)=>(`+id+`)\(`+regexp.QuoteMeta(account[4])+`\)\.userId\?\?null\)`, ".userId??null)", "user atom")
	if err != nil || user[2] != user[3] {
		return nil, errors.New("native user atom contract missing or ambiguous")
	}
	ready, err := nativeSemanticMatch(source, `(`+id+`)=`+id+`\(`+id+`,\(\{get:`+id+`\}\)=>\{let (`+id+`)=`+id+`\(`+regexp.QuoteMeta(account[4])+`\);return!`+id+`\.authLoading&&\(`+id+`\.authMethod!==[\x60'"]chatgpt[\x60'"]\|\|`+id+`\.authenticatedAccountId!=null&&`+id+`\.userId!=null\)\}\)`, "authLoading&&", "authentication readiness")
	if err != nil {
		return nil, err
	}
	api, err := nativeSemanticMatch(source, `(`+id+`)=new (`+id+`)\(\{getAdditionalHeaders:`+id+`,getRequestSignal:`+id+`\}\)`, "getAdditionalHeaders:", "authenticated API")
	if err != nil {
		return nil, err
	}
	apiClass, err := nativeClass(source, api[2])
	if err != nil || !strings.Contains(apiClass, "safeGet(") || !strings.Contains(apiClass, "safePost(") || !strings.Contains(apiClass, "safePatch(") {
		return nil, errors.New("native authenticated API methods changed")
	}
	factory, err := nativeSemanticMatch(source, `function (`+id+`)\((`+id+`)\)\{let `+id+`=`+id+`\.get\(`+id+`,void 0\),`+id+`=`+id+`\.get\(`+regexp.QuoteMeta(account[1])+`\),`+id+`=`+id+`\.get\(`+regexp.QuoteMeta(user[1])+`\);`, ".cache?.accountId", "account cache")
	if err != nil {
		return nil, err
	}
	factoryStart := strings.Index(source, factory[0])
	factoryBody, ok := nativeJSBody(source, factoryStart+strings.IndexByte(factory[0], '{'))
	if !ok || !strings.Contains(factoryBody, ".cache?.accountId") || !strings.Contains(factoryBody, ".cache.userId") || !strings.Contains(factoryBody, ".cache.disposed") || !strings.Contains(factoryBody, ".watch(") || !strings.Contains(factoryBody, ".stopWatching=") {
		return nil, errors.New("native account cache ownership changed")
	}
	cache, err := uniqueNativeMatch(factoryBody, `new (`+id+`)\(`, "cache constructor")
	if err != nil {
		return nil, err
	}
	cacheClass, err := nativeClass(source, cache[1])
	if err != nil || !strings.Contains(cacheClass, "readRoom(") || !strings.Contains(cacheClass, "readHistory(") || !strings.Contains(cacheClass, "this.abort.signal") || !regexp.MustCompile(`read\(`+id+`,`+id+`,`+id+`,`+id+`,`+id+`,`+id+`\)\{`).MatchString(cacheClass) {
		return nil, errors.New("native six-argument messaging read contract changed")
	}
	executor, err := nativeSemanticMatch(source, `function (`+id+`)\(\)\{return `+id+`\?\.tppLocalExecutor\}`, "?.tppLocalExecutor", "local executor")
	if err != nil {
		return nil, err
	}
	bindings := &NativeBindings{}
	for local, target := range map[string]*string{api[1]: &bindings.API, account[1]: &bindings.Account, user[1]: &bindings.User, ready[1]: &bindings.Ready, factory[1]: &bindings.Cache, executor[1]: &bindings.Executor} {
		export, err := nativeExport(source, local)
		if err != nil {
			return nil, err
		}
		*target = export
	}
	return bindings, nil
}

func resolveNativeBindings(modules map[string]string) (string, *NativeBindings, error) {
	names := make([]string, 0, len(modules))
	for name := range modules {
		names = append(names, name)
	}
	sort.Strings(names)
	var initial string
	var bindings *NativeBindings
	for _, name := range names {
		if !strings.HasPrefix(name, "app-initial-") {
			continue
		}
		if initial != "" {
			return "", nil, errors.New("native initial module ambiguous")
		}
		initial = name
		var err error
		bindings, err = resolveNativeInitial(modules[name])
		if err != nil {
			return "", nil, err
		}
	}
	if bindings == nil {
		return "", nil, errors.New("native initial module unavailable")
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "app-shared-") {
			continue
		}
		source := modules[name]
		instances := regexp.MustCompile(`(?:^|[,;])(`+jsIdentifier+`)=(`+jsIdentifier+`)\.getInstance\(\)`).FindAllStringSubmatch(source, -1)
		for _, instance := range instances {
			body, err := nativeClass(source, instance[2])
			if err != nil || !strings.Contains(body, "dispatchMessage(") || !strings.Contains(body, "subscribe(") || !strings.Contains(body, "deliverMessage(") || !strings.Contains(body, ".postMessage(") {
				continue
			}
			export, err := nativeExport(source, instance[1])
			if err != nil {
				continue
			}
			if bindings.Bus != "" {
				return "", nil, errors.New("native message bus contract ambiguous")
			}
			bindings.Bus, bindings.BusAsset = export, name
		}
	}
	if bindings.Bus == "" {
		return "", nil, errors.New("native message bus contract unavailable")
	}
	if err := bindings.validate(); err != nil {
		return "", nil, err
	}
	return initial, bindings, nil
}
