// Package core contains the target-agnostic code to define and run a bridge
package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/git-bug/gitconfig"

	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/repository"
)

var ErrImportNotSupported = errors.New("import is not supported")
var ErrExportNotSupported = errors.New("export is not supported")
var ErrBridgeRunning = errors.New("a bridge is already running in this repository")

const (
	ConfigKeyTarget = "target"
	// ConfigKeySync enables sync: mirroring everything, from any forge.
	ConfigKeySync = "sync"
	// ConfigKeyDefaultLogin is the login posting for people without a token.
	ConfigKeyDefaultLogin = "default-login"
	// ConfigKeyLoginPrefix, followed by an identity id, links that identity
	// to the login it has on the forge: login-<identity> = <login>.
	ConfigKeyLoginPrefix = "login-"
	// ConfigKeyAssignees is how the forge holds git-bug's assignee:<login>
	// labels: ModeNative (the default) or ModeLabels. Every clone must hold
	// them the same way, or each sync undoes the others': WithAssignees
	// gives it from a shared configuration.
	ConfigKeyAssignees = "assignees"
	// ConfigKeyKinds is how the forge holds kinds: ModeNative (the default,
	// as issue types where the forge has them) or ModeLabels.
	ConfigKeyKinds = "kinds"
	// ConfigKeySprints is how the forge holds sprints: ModeNative (the
	// default, as milestones where the forge has them) or ModeOff.
	ConfigKeySprints = "sprints"
	// ConfigKeyKindTypes maps kinds to the forge's types.
	ConfigKeyKindTypes = "kind-types"

	// ModeNative holds it as the forge's own field.
	ModeNative = "native"
	// ModeLabels holds it as labels on the forge.
	ModeLabels = "labels"
	// ModeOff neither reads nor writes the forge's own field.
	ModeOff = "off"

	// AssigneesNative holds assignees as the forge's assignees.
	AssigneesNative = ModeNative
	// AssigneesLabels holds them as labels on the forge too.
	AssigneesLabels = ModeLabels

	MetaKeyOrigin = "origin"

	// the configuration of bridge <name> is in [git-bug "bridge.<name>"]
	bridgeConfigSection          = "git-bug"
	bridgeConfigSubsectionPrefix = "bridge."

	configKeyLastImportTime = "lastImportTime"
	// configKeyLastSyncTime is apart from the import time, as an older bridge
	// may have imported only part of a project.
	configKeyLastSyncTime = "lastSyncTime"

	// bridgeLockFile is locked while a bridge imports or exports, so that only
	// one runs at a time in a repository: concurrent runs would import or export
	// the same items twice. Every bridge shares it, as two bridges can target the
	// same remote, and the import and export of a bridge race too.
	bridgeLockFile = "bridge.lock"
)

// bridgeConfigKey returns the key of the configuration entry name of the bridge.
func bridgeConfigKey(bridge, name string) string {
	return bridgeConfigSection + "." + bridgeConfigSubsectionPrefix + bridge + "." + name
}

var bridgeImpl map[string]reflect.Type
var bridgeLoginMetaKey map[string]string

// Bridge is a wrapper around a BridgeImpl that will bind low-level
// implementation with utility code to provide high-level functions.
type Bridge struct {
	Name           string
	repo           *cache.RepoCache
	impl           BridgeImpl
	importer       Importer
	exporter       Exporter
	conf           Configuration
	initImportDone bool
	initExportDone bool
	// opts change the SyncOptions of its engines, see Use
	opts []Option
}

// Use sets options of the sync engines the bridge creates from now on, for
// its pulls and pushes, beyond what its configuration gives.
func (b *Bridge) Use(opts ...Option) {
	b.opts = append(b.opts, opts...)
}

// Register will register a new BridgeImpl
func Register(impl BridgeImpl) {
	if bridgeImpl == nil {
		bridgeImpl = make(map[string]reflect.Type)
	}
	if bridgeLoginMetaKey == nil {
		bridgeLoginMetaKey = make(map[string]string)
	}
	bridgeImpl[impl.Target()] = reflect.TypeOf(impl).Elem()
	bridgeLoginMetaKey[impl.Target()] = impl.LoginMetaKey()
}

// Targets return all known bridge implementation target
func Targets() []string {
	var result []string

	for key := range bridgeImpl {
		result = append(result, key)
	}

	sort.Strings(result)

	return result
}

// TargetExist return true if the given target has a bridge implementation
func TargetExist(target string) bool {
	_, ok := bridgeImpl[target]
	return ok
}

// LoginMetaKey return the metadata key used to store the remote bug-tracker login
// on the user identity. The corresponding value is used to match identities and
// credentials.
func LoginMetaKey(target string) (string, error) {
	metaKey, ok := bridgeLoginMetaKey[target]
	if !ok {
		return "", fmt.Errorf("unknown bridge target %v", target)
	}

	return metaKey, nil
}

// Instantiate a new Bridge for a repo, from the given target and name
func NewBridge(repo *cache.RepoCache, target string, name string) (*Bridge, error) {
	implType, ok := bridgeImpl[target]
	if !ok {
		return nil, fmt.Errorf("unknown bridge target %v", target)
	}

	impl := reflect.New(implType).Interface().(BridgeImpl)

	bridge := &Bridge{
		Name: name,
		repo: repo,
		impl: impl,
	}

	return bridge, nil
}

// LoadBridge instantiate a new bridge from a repo configuration
func LoadBridge(repo *cache.RepoCache, name string) (*Bridge, error) {
	conf, err := loadConfig(repo, name)
	if err != nil {
		return nil, err
	}

	target := conf[ConfigKeyTarget]
	bridge, err := NewBridge(repo, target, name)
	if err != nil {
		return nil, err
	}

	err = bridge.impl.ValidateConfig(conf)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	if !bridge.SupportsSync() && conf[ConfigKeySync] != "" {
		return nil, fmt.Errorf("invalid configuration: the %s bridge doesn't support %s", target, ConfigKeySync)
	}

	// will avoid reloading configuration before an export or import call
	bridge.conf = conf
	return bridge, nil
}

// Attempt to retrieve a default bridge for the given repo. If zero or multiple
// bridge exist, it fails.
func DefaultBridge(repo *cache.RepoCache) (*Bridge, error) {
	bridges, err := ConfiguredBridges(repo)
	if err != nil {
		return nil, err
	}

	if len(bridges) == 0 {
		return nil, fmt.Errorf("no configured bridge")
	}

	if len(bridges) > 1 {
		return nil, fmt.Errorf("multiple bridge are configured, you need to select one explicitly")
	}

	return LoadBridge(repo, bridges[0])
}

// ConfiguredBridges return the list of bridge that are configured for the given
// repo
func ConfiguredBridges(repo repository.RepoConfig) ([]string, error) {
	cfg, err := repo.Config().Read()
	if err != nil {
		return nil, fmt.Errorf("can't read configured bridges: %w", err)
	}

	var result []string
	for _, sub := range cfg.Subsections(bridgeConfigSection) {
		if name, ok := strings.CutPrefix(sub, bridgeConfigSubsectionPrefix); ok {
			result = append(result, name)
		}
	}

	return result, nil
}

// Check if a bridge exist
func BridgeExist(repo repository.RepoConfig, name string) bool {
	cfg, err := repo.Config().Read()
	if err != nil {
		return false
	}

	for range cfg.Subsection(bridgeConfigSection, bridgeConfigSubsectionPrefix+name) {
		return true
	}
	return false
}

// Remove a configured bridge
func RemoveBridge(repo repository.RepoConfig, name string) error {
	re := regexp.MustCompile(`^[a-zA-Z0-9]+`)

	if !re.MatchString(name) {
		return fmt.Errorf("bad bridge fullname: %s", name)
	}

	return repo.Config().Update(func(f *gitconfig.File) error {
		_, err := f.RemoveSubsection(bridgeConfigSection, bridgeConfigSubsectionPrefix+name)
		return err
	})
}

// Configure run the target specific configuration process
func (b *Bridge) Configure(params BridgeParams, interactive bool) error {
	validateParams(params, b.impl)

	conf, err := b.impl.Configure(b.repo, params, interactive)
	if err != nil {
		return err
	}

	err = b.impl.ValidateConfig(conf)
	if err != nil {
		return fmt.Errorf("invalid configuration: %v", err)
	}

	b.conf = conf
	return b.storeConfig(conf)
}

func validateParams(params BridgeParams, impl BridgeImpl) {
	validParams := impl.ValidParams()

	paramsValue := reflect.ValueOf(params)
	paramsType := paramsValue.Type()

	for i := 0; i < paramsValue.NumField(); i++ {
		name := paramsType.Field(i).Name
		val := paramsValue.Field(i).Interface().(string)
		_, valid := validParams[name]
		if val != "" && !valid {
			_, _ = fmt.Fprintln(os.Stderr, params.fieldWarning(name, impl.Target()))
		}
	}
}

func (b *Bridge) storeConfig(conf Configuration) error {
	err := b.repo.Config().Update(func(f *gitconfig.File) error {
		for key, val := range conf {
			if err := f.ReplaceAll(bridgeConfigKey(b.Name, key), val); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("error while storing bridge configuration: %w", err)
	}
	return nil
}

func (b *Bridge) ensureConfig() error {
	if b.conf == nil {
		conf, err := loadConfig(b.repo, b.Name)
		if err != nil {
			return err
		}
		b.conf = conf
	}

	return nil
}

func loadConfig(repo repository.RepoConfig, name string) (Configuration, error) {
	cfg, err := repo.Config().Read()
	if err != nil {
		return nil, fmt.Errorf("error while reading bridge configuration: %w", err)
	}

	result := make(Configuration)
	for e := range cfg.Subsection(bridgeConfigSection, bridgeConfigSubsectionPrefix+name) {
		result[e.Key.Name] = e.Value
	}

	return result, nil
}

func (b *Bridge) getImporter() Importer {
	if b.importer == nil {
		b.importer = b.impl.NewImporter()
	}

	return b.importer
}

func (b *Bridge) getExporter() Exporter {
	if b.exporter == nil {
		b.exporter = b.impl.NewExporter()
	}

	return b.exporter
}

func (b *Bridge) ensureImportInit(ctx context.Context) error {
	if b.initImportDone {
		return nil
	}

	importer := b.getImporter()
	if importer != nil {
		err := importer.Init(ctx, b.repo, b.conf)
		if err != nil {
			return err
		}
	}

	b.initImportDone = true
	return nil
}

func (b *Bridge) ensureExportInit(ctx context.Context) error {
	if b.initExportDone {
		return nil
	}

	exporter := b.getExporter()
	if exporter != nil {
		err := exporter.Init(ctx, b.repo, b.conf)
		if err != nil {
			return err
		}
	}

	b.initExportDone = true
	return nil
}

func (b *Bridge) ImportAllSince(ctx context.Context, since time.Time) (<-chan ImportResult, error) {
	// 5 seconds before the actual start just to be sure.
	importStartTime := time.Now().Add(-5 * time.Second)

	unlock, err := lockBridges(b.repo)
	if err != nil {
		return nil, err
	}

	err = b.ensureConfig()
	if err != nil {
		unlock()
		return nil, err
	}

	events, err := b.importEvents(ctx, since)
	if err != nil {
		unlock()
		return nil, err
	}

	out := make(chan ImportResult)
	go func() {
		defer close(out)
		defer unlock()
		noError := true

		// relay all events while checking that everything went well
		for event := range events {
			if event.Event == ImportEventError {
				noError = false
			}
			out <- event
		}

		// store the last import time ONLY if no error happened
		if noError {
			_ = b.storeImportTime(importStartTime)
		}
	}()

	return out, nil
}

func (b *Bridge) importEvents(ctx context.Context, since time.Time) (<-chan ImportResult, error) {
	if _, ported := b.impl.(RemoteBridge); ported {
		e, err := b.newEngine(ctx, false)
		if err != nil {
			return nil, err
		}
		return e.Pull(ctx, since), nil
	}

	importer := b.getImporter()
	if importer == nil {
		return nil, ErrImportNotSupported
	}
	if err := b.ensureImportInit(ctx); err != nil {
		return nil, err
	}
	return importer.ImportAll(ctx, b.repo, since)
}

func (b *Bridge) exportEvents(ctx context.Context, since time.Time) (<-chan ExportResult, error) {
	if _, ported := b.impl.(RemoteBridge); ported {
		e, err := b.newEngine(ctx, false)
		if err != nil {
			return nil, err
		}
		return e.Push(ctx), nil
	}

	exporter := b.getExporter()
	if exporter == nil {
		return nil, ErrExportNotSupported
	}
	if err := b.ensureExportInit(ctx); err != nil {
		return nil, err
	}
	return exporter.ExportAll(ctx, b.repo, since)
}

// newEngine returns the sync engine of a bridge ported to it.
func (b *Bridge) newEngine(ctx context.Context, dryRun bool) (*Engine, error) {
	var err error
	foreign := false
	if v, ok := b.conf[ConfigKeySync]; ok {
		if foreign, err = strconv.ParseBool(v); err != nil {
			return nil, fmt.Errorf("%s: %w", ConfigKeySync, err)
		}
	}
	links := map[string]string{}
	for k, login := range b.conf {
		prefix, ok := strings.CutPrefix(k, ConfigKeyLoginPrefix)
		if !ok {
			continue
		}
		login = strings.ToLower(login)
		if other, ok := links[login]; ok {
			return nil, fmt.Errorf("%s%s and %s%s: the login %s is linked to two identities", ConfigKeyLoginPrefix, other, ConfigKeyLoginPrefix, prefix, login)
		}
		links[login] = prefix
	}
	opts := SyncOptions{
		Foreign:      foreign,
		DryRun:       dryRun,
		DefaultLogin: b.conf[ConfigKeyDefaultLogin],
		LoginKey:     b.impl.LoginMetaKey(),
		Links:        links,
	}
	for _, o := range b.opts {
		o(&opts)
	}

	conf := maps.Clone(b.conf)
	if conf == nil {
		conf = map[string]string{}
	}
	for k, v := range opts.Config {
		if v != "" {
			conf[k] = v
		}
	}
	for key, modes := range map[string][]string{
		ConfigKeyAssignees: {ModeNative, ModeLabels},
		ConfigKeyKinds:     {ModeNative, ModeLabels},
		ConfigKeySprints:   {ModeNative, ModeOff},
	} {
		if v := conf[key]; v != "" && !slices.Contains(modes, v) {
			return nil, fmt.Errorf("%s: %q is neither %s", key, v, strings.Join(modes, " nor "))
		}
	}
	opts.Assignees = conf[ConfigKeyAssignees]
	remote, err := b.impl.(RemoteBridge).NewRemote(ctx, b.repo, conf)
	if err != nil {
		return nil, err
	}
	return NewEngine(ctx, b.repo, remote, opts)
}

// SupportsSync tells whether the bridge can take part in `bridge sync`.
func (b *Bridge) SupportsSync() bool {
	_, ported := b.impl.(RemoteBridge)
	return ported
}

// HasInstances tells whether the target runs on several instances, given by
// --base-url, whose credentials record their instance.
func (b *Bridge) HasInstances() bool {
	_, ok := b.impl.ValidParams()["BaseURL"]
	return ok
}

// IsSync tells whether the bridge takes part in `bridge sync`.
func (b *Bridge) IsSync() bool {
	v, _ := strconv.ParseBool(b.conf[ConfigKeySync])
	return v
}

func (b *Bridge) lastImportTime() time.Time {
	return b.configTime(configKeyLastImportTime)
}

func (b *Bridge) configTime(key string) time.Time {
	cfg, err := b.repo.Config().Read()
	if err != nil {
		return time.Time{}
	}
	lastImport, err := cfg.Int(bridgeConfigKey(b.Name, key), 0)
	if err != nil || lastImport == 0 {
		return time.Time{}
	}
	return time.Unix(lastImport, 0)
}

func (b *Bridge) storeImportTime(t time.Time) error {
	return b.storeConfigTime(configKeyLastImportTime, t)
}

func (b *Bridge) storeConfigTime(key string, t time.Time) error {
	return SetBridgeConfig(b.repo, b.Name, key, strconv.FormatInt(t.Unix(), 10))
}

// SetBridgeConfig sets a configuration key of a bridge.
func SetBridgeConfig(repo repository.RepoConfig, bridge, key, value string) error {
	return repo.Config().Update(func(f *gitconfig.File) error {
		return f.ReplaceAll(bridgeConfigKey(bridge, key), value)
	})
}

func (b *Bridge) ImportAll(ctx context.Context) (<-chan ImportResult, error) {
	// If possible, restart from the last import time
	return b.ImportAllSince(ctx, b.lastImportTime())
}

func (b *Bridge) ExportAll(ctx context.Context, since time.Time) (<-chan ExportResult, error) {
	unlock, err := lockBridges(b.repo)
	if err != nil {
		return nil, err
	}

	err = b.ensureConfig()
	if err != nil {
		unlock()
		return nil, err
	}

	events, err := b.exportEvents(ctx, since)
	if err != nil {
		unlock()
		return nil, err
	}

	out := make(chan ExportResult)
	go func() {
		defer close(out)
		defer unlock()

		for event := range events {
			out <- event
		}
	}()

	return out, nil
}

// lockBridges takes the bridge lock of the repository, see bridgeLockFile, and
// returns the function releasing it. Like git with its index.lock, the lock is a
// file created exclusively: if it exists, it fails with ErrBridgeRunning rather
// than waiting.
func lockBridges(repo repository.RepoStorage) (unlock func(), err error) {
	storage := repo.LocalStorage()

	f, err := storage.OpenFile(bridgeLockFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if errors.Is(err, os.ErrExist) {
		path := filepath.Join(storage.Root(), bridgeLockFile)
		return nil, fmt.Errorf("%w: '%s' exists.\n\n"+
			"If no other git-bug bridge is running, a previous one may have crashed:\n"+
			"remove the file manually to continue", ErrBridgeRunning, path)
	}
	if err != nil {
		return nil, err
	}

	err = f.Close()
	if err != nil {
		_ = storage.Remove(bridgeLockFile)
		return nil, err
	}

	return func() { _ = storage.Remove(bridgeLockFile) }, nil
}
