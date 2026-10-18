package dots

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

//go:embed executor_primary.js
var executorPrimaryJS string

func executorSystemDot(s *Settings) (Dot, error) {
	var result Dot
	count := 0
	for _, dot := range s.Dots {
		if dot.OwnerAlias == s.SystemAlias {
			result = dot
			count++
		}
	}
	if count != 1 {
		return Dot{}, errors.New("native primary Dot unavailable")
	}
	return result, nil
}

func primaryExecutorEnvironment(ctx context.Context, s *Settings, e NativeEvaluator) (string, error) {
	dot, err := executorSystemDot(s)
	if err != nil {
		return "", err
	}
	expression := strings.ReplaceAll(nativeAsset(executorPrimaryJS, s), "PROFILE_PLACEHOLDER", nativeJSON(dot.ProfileID))
	value, err := nativeOne(ctx, e, expression)
	if err != nil {
		return "", err
	}
	state, ok := value.(map[string]any)
	environment, _ := state["environment_id"].(string)
	if !ok || state["ready"] != true || state["enabled"] != true || !validExecutorEnvironment(environment) {
		return "", errors.New("native local executor is not ready")
	}
	return environment, nil
}

type secondaryExecutorWorker struct {
	owner            Account
	broker           *executorRegistryBroker
	child            executorProcess
	record           map[string]any
	connectAttempted bool
}

func startSecondaryExecutor(ctx context.Context, s *Settings, owner Account, primary string, deps executorDependencies) (*secondaryExecutorWorker, error) {
	profiles := map[string]bool{}
	for _, dot := range s.Dots {
		if dot.OwnerAlias == owner.Alias {
			profiles[dot.ProfileID] = true
		}
	}
	if owner.Alias == s.SystemAlias || !aliasPattern.MatchString(owner.Alias) || len(profiles) != 1 || !validExecutorEnvironment(primary) {
		return nil, errors.New("secondary executor identity invalid")
	}
	profile := ""
	for id := range profiles {
		profile = id
	}
	root, err := executorDirectory(s, "executors", owner.Alias)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"home", "work", "tmp"} {
		if _, err := executorDirectory(s, "executors", owner.Alias, name); err != nil {
			return nil, err
		}
	}
	identity := filepath.Join(root, "installation.json")
	installation := ""
	if _, err := os.Lstat(identity); err == nil {
		value, err := executorRead(identity, s.RuntimeRoot)
		if err != nil {
			return nil, err
		}
		installation, _ = value["id"].(string)
		if !nativeValidUUID(installation) {
			return nil, errors.New("invalid executor installation identity")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	} else {
		installation, err = nativeUUID()
		if err != nil {
			return nil, err
		}
		if err := installSave(identity, map[string]any{"id": installation}); err != nil {
			return nil, err
		}
	}
	display, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	environment, err := registerExecutor(ctx, deps.request, owner, installation, display)
	if err != nil {
		return nil, err
	}
	if environment == primary {
		return nil, errors.New("secondary executor registration reused primary identity")
	}
	broker, err := newExecutorRegistryBroker(s, owner, environment, owner.Alias, deps.request)
	if err != nil {
		return nil, err
	}
	broker.start()
	child, err := deps.launch(s, root, broker.port(), environment, broker.capability, false, false)
	if err != nil {
		broker.close()
		return nil, err
	}
	record := map[string]any{"owner_alias": owner.Alias, "profile_id": profile, "environment_id": environment, "primary_environment_id": primary, "active": true, "registered": false}
	return &secondaryExecutorWorker{owner: owner, broker: broker, child: child, record: record}, nil
}
func refreshSecondaryExecutor(ctx context.Context, s *Settings, worker *secondaryExecutorWorker, deps executorDependencies) map[string]any {
	_, running := worker.child.poll()
	stopped, registered := worker.broker.snapshot()
	worker.record["active"] = running && !stopped
	worker.record["registered"] = registered
	if running && !stopped && registered && !worker.connectAttempted {
		worker.connectAttempted = true
		value, err := deps.get(ctx, worker.owner, "/backend-api/tbo/primary", nil)
		profile, _ := value["profile"].(map[string]any)
		id, _ := profile["id"].(string)
		root, _ := profile["active_root_thread_id"].(string)
		if err == nil && (id != worker.record["profile_id"] || root == "" || len(root) > 128) {
			err = errors.New("secondary profile changed")
		}
		if err == nil {
			path := "/backend-api/tbo/" + url.PathEscape(id) + "/computers/" + worker.record["environment_id"].(string) + "/connect?" + url.Values{"expected_thread_id": []string{root}}.Encode()
			status, _, _, requestErr := deps.request(ctx, worker.owner, "chatgpt.com", "POST", path, nil)
			err = requestErr
			if err == nil {
				worker.record["connect_http"] = status
			}
		}
		if err != nil {
			worker.record["connect_error"] = "ValueError"
		}
	}
	return worker.record
}
func closeSecondaryExecutor(worker *secondaryExecutorWorker) error {
	err := closeExecutor(worker.child)
	worker.broker.close()
	worker.record["active"] = false
	return err
}

// SecondaryExecutorMapping returns only private, active, registered owner-bound state.
func SecondaryExecutorMapping(s *Settings, alias string) (map[string]any, error) {
	if s == nil || alias == s.SystemAlias || !aliasPattern.MatchString(alias) {
		return nil, nil
	}
	state, err := executorRead(filepath.Join(s.RuntimeRoot, "executors", "state.json"), s.RuntimeRoot)
	if err != nil {
		return nil, nil
	}
	owners, _ := state["owners"].(map[string]any)
	row, ok := owners[alias].(map[string]any)
	if !ok {
		return nil, nil
	}
	profiles := map[string]bool{}
	for _, dot := range s.Dots {
		if dot.OwnerAlias == alias {
			profiles[dot.ProfileID] = true
		}
	}
	profile, _ := row["profile_id"].(string)
	environment, _ := row["environment_id"].(string)
	primary, _ := row["primary_environment_id"].(string)
	if row["owner_alias"] != alias || !profiles[profile] || row["active"] != true || row["registered"] != true || !validExecutorEnvironment(environment) || !validExecutorEnvironment(primary) {
		return nil, nil
	}
	return row, nil
}

// ProjectExecutorComputers aliases only the installation ID; actual status remains unchanged.
func ProjectExecutorComputers(s *Settings, alias, profile string, data []byte) ([]byte, error) {
	row, err := SecondaryExecutorMapping(s, alias)
	if err != nil {
		return nil, err
	}
	if row == nil || row["profile_id"] != profile {
		return data, nil
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil || value == nil {
		return nil, errors.New("owner computer list invalid")
	}
	computers, ok := value["computers"].([]any)
	if !ok {
		return nil, errors.New("owner computer list invalid")
	}
	for _, item := range computers {
		computer, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("owner computer list invalid")
		}
		if computer["kind"] == "user" && computer["environment_id"] == row["environment_id"] {
			computer["environment_id"] = row["primary_environment_id"]
		}
	}
	return json.Marshal(value)
}

// RunSecondaryExecutors owns one independent local executor per secondary Dot owner.
func RunSecondaryExecutors(ctx context.Context, s *Settings, c *CatalogueClient, e NativeEvaluator) error {
	return runSecondaryExecutors(ctx, s, c, e, executorDefaults, nil)
}
func runSecondaryExecutors(ctx context.Context, s *Settings, c *CatalogueClient, e NativeEvaluator, defaults func(*Settings, *CatalogueClient, string) executorDependencies, primaryLookup func(context.Context, *Settings, NativeEvaluator) (string, error)) (result error) {
	if s == nil || s.IntegrationMode != "native" {
		return errors.New("secondary executors require native integration")
	}
	root, err := executorDirectory(s, "executors")
	if err != nil {
		return err
	}
	lock, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, filepath.Join(root, "process.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if primaryLookup == nil {
		primaryLookup = primaryExecutorEnvironment
	}
	if e == nil {
		e = NewNativeEvaluator(s)
	}
	primary := ""
	for attempt := 0; attempt < 30; attempt++ {
		if ctx.Err() != nil {
			return nil
		}
		primary, err = primaryLookup(ctx, s, e)
		if err == nil {
			break
		}
		if waitContext(ctx, 2*time.Second) != nil {
			return nil
		}
	}
	if primary == "" {
		return errors.New("native executor startup timed out")
	}
	common := defaults(s, c, s.SecondaryAlias)
	inventory, err := common.inventory()
	if err != nil {
		return err
	}
	if inventory.System.Alias != s.SystemAlias {
		return errors.New("native account changed")
	}
	workers := []*secondaryExecutorWorker{}
	failures := map[string]any{}
	save := func() error {
		owners := map[string]any{}
		for _, worker := range workers {
			owners[worker.owner.Alias] = worker.record
		}
		return installSave(filepath.Join(root, "state.json"), map[string]any{"owners": owners, "failures": failures})
	}
	defer func() {
		for _, worker := range workers {
			if err := closeSecondaryExecutor(worker); result == nil {
				result = err
			}
		}
		if err := save(); result == nil {
			result = err
		}
	}()
	aliases := map[string]bool{}
	for _, dot := range s.Dots {
		if dot.OwnerAlias != s.SystemAlias {
			aliases[dot.OwnerAlias] = true
		}
	}
	order := make([]string, 0, len(aliases))
	for alias := range aliases {
		order = append(order, alias)
	}
	sort.Strings(order)
	installations := map[string]bool{}
	for _, alias := range order {
		owner, err := executorSecondary(s, inventory, alias)
		if err != nil {
			failures[alias] = "ValueError"
			continue
		}
		// Existing installations must remain unique across owners, including migrated state.
		identity := filepath.Join(root, alias, "installation.json")
		if value, readErr := executorRead(identity, s.RuntimeRoot); readErr == nil {
			installation, _ := value["id"].(string)
			if installations[installation] {
				failures[alias] = "ValueError"
				continue
			}
			installations[installation] = true
		}
		worker, err := startSecondaryExecutor(ctx, s, owner, primary, defaults(s, c, alias))
		if err != nil {
			failures[alias] = "ValueError"
			continue
		}
		workers = append(workers, worker)
	}
	if err := save(); err != nil {
		return err
	}
	for waitContext(ctx, 2*time.Second) == nil {
		current, err := primaryLookup(ctx, s, e)
		if err != nil {
			return err
		}
		if current != primary {
			return errors.New("native executor identity changed")
		}
		inventory, err := common.inventory()
		if err != nil {
			return err
		}
		if inventory.System.Alias != s.SystemAlias {
			return errors.New("native account changed")
		}
		for _, worker := range workers {
			refreshSecondaryExecutor(ctx, s, worker, defaults(s, c, worker.owner.Alias))
		}
		if err := save(); err != nil {
			return err
		}
	}
	return nil
}

// ReconcileSecondaryExecutors is the long-running helper entry point.
func ReconcileSecondaryExecutors(ctx context.Context, s *Settings, c *CatalogueClient, e NativeEvaluator) error {
	return RunSecondaryExecutors(ctx, s, c, e)
}
