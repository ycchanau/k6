package js

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"

	"github.com/dop251/goja"
	"github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"gopkg.in/guregu/null.v3"

	"go.k6.io/k6/js/common"
	"go.k6.io/k6/js/compiler"
	"go.k6.io/k6/js/eventloop"
	"go.k6.io/k6/lib"
	"go.k6.io/k6/lib/consts"
	"go.k6.io/k6/loader"
)

// A Bundle is a self-contained bundle of scripts and resources.
// You can use this to produce identical BundleInstance objects.
type Bundle struct {
	sourceData *loader.SourceData
	Options    lib.Options

	CompatibilityMode lib.CompatibilityMode // parsed value
	preInitState      *lib.TestPreInitState

	filesystems map[string]afero.Fs
	pwd         *url.URL

	callableExports map[string]struct{}
	moduleResolver  *moduleResolver
}

// A BundleInstance is a self-contained instance of a Bundle.
type BundleInstance struct {
	Runtime *goja.Runtime

	// TODO: maybe just have a reference to the Bundle? or save and pass rtOpts?
	env map[string]string

	mainModuleInstance moduleInstance
	moduleVUImpl       *moduleVUImpl
}

func (bi *BundleInstance) getCallableExport(name string) goja.Callable {
	fn, ok := goja.AssertFunction(bi.mainModuleInstance.exports().Get(name))
	_ = ok // TODO maybe return it
	return fn
}

func (bi *BundleInstance) getExported(name string) goja.Value {
	return bi.mainModuleInstance.exports().ToObject(bi.Runtime).Get(name)
}

// NewBundle creates a new bundle from a source file and a filesystem.
func NewBundle(
	piState *lib.TestPreInitState, src *loader.SourceData, filesystems map[string]afero.Fs,
) (*Bundle, error) {
	return newBundle(piState, src, filesystems, lib.Options{}, true)
}

func newBundle(
	piState *lib.TestPreInitState, src *loader.SourceData, filesystems map[string]afero.Fs,
	options lib.Options, updateOptions bool, // TODO: try to figure out a way to not need both
) (*Bundle, error) {
	compatMode, err := lib.ValidateCompatibilityMode(piState.RuntimeOptions.CompatibilityMode.String)
	if err != nil {
		return nil, err
	}

	// Make a bundle, instantiate it into a throwaway VM to populate caches.
	bundle := &Bundle{
		sourceData:        src,
		Options:           options,
		CompatibilityMode: compatMode,
		callableExports:   make(map[string]struct{}),
		filesystems:       filesystems,
		pwd:               loader.Dir(src.URL),
		preInitState:      piState,
	}
	c := bundle.newCompiler(piState.Logger)
	bundle.moduleResolver = newModuleResolution(getJSModules(), generateCJSLoad(bundle, c))

	if err = bundle.moduleResolver.setMain(src, c); err != nil {
		return nil, err
	}
	// Instantiate the bundle into a new VM using a bound init context. This uses a context with a
	// runtime, but no state, to allow module-provided types to function within the init context.
	// TODO use a real context
	vuImpl := &moduleVUImpl{ctx: context.Background(), runtime: goja.New()}
	vuImpl.eventLoop = eventloop.New(vuImpl)
	instance, err := bundle.instantiate(vuImpl, 0)
	if err != nil {
		return nil, err
	}

	err = bundle.populateExports(piState.Logger, updateOptions, instance)
	if err != nil {
		return nil, err
	}

	return bundle, nil
}

// NewBundleFromArchive creates a new bundle from an lib.Archive.
func NewBundleFromArchive(piState *lib.TestPreInitState, arc *lib.Archive) (*Bundle, error) {
	if arc.Type != "js" {
		return nil, fmt.Errorf("expected bundle type 'js', got '%s'", arc.Type)
	}

	if !piState.RuntimeOptions.CompatibilityMode.Valid {
		// `k6 run --compatibility-mode=whatever archive.tar` should override
		// whatever value is in the archive
		piState.RuntimeOptions.CompatibilityMode = null.StringFrom(arc.CompatibilityMode)
	}
	env := arc.Env
	if env == nil {
		// Older archives (<=0.20.0) don't have an "env" property
		env = make(map[string]string)
	}
	for k, v := range piState.RuntimeOptions.Env {
		env[k] = v
	}
	piState.RuntimeOptions.Env = env

	return newBundle(piState, &loader.SourceData{
		Data: arc.Data,
		URL:  arc.FilenameURL,
	}, arc.Filesystems, arc.Options, false)
}

func (b *Bundle) makeArchive() *lib.Archive {
	arc := &lib.Archive{
		Type:              "js",
		Filesystems:       b.filesystems,
		Options:           b.Options,
		FilenameURL:       b.sourceData.URL,
		Data:              b.sourceData.Data,
		PwdURL:            b.pwd,
		Env:               make(map[string]string, len(b.preInitState.RuntimeOptions.Env)),
		CompatibilityMode: b.CompatibilityMode.String(),
		K6Version:         consts.Version,
		Goos:              runtime.GOOS,
	}
	// Copy env so changes in the archive are not reflected in the source Bundle
	for k, v := range b.preInitState.RuntimeOptions.Env {
		arc.Env[k] = v
	}

	return arc
}

// populateExports validates and extracts exported objects
func (b *Bundle) populateExports(logger logrus.FieldLogger, updateOptions bool, instance moduleInstance) error {
	exports := instance.exports()
	if exports == nil {
		return errors.New("exports must be an object")
	}
	for _, k := range exports.Keys() {
		v := exports.Get(k)
		if _, ok := goja.AssertFunction(v); ok && k != consts.Options {
			b.callableExports[k] = struct{}{}
			continue
		}
		switch k {
		case consts.Options:
			if !updateOptions {
				continue
			}
			data, err := json.Marshal(v.Export())
			if err != nil {
				return fmt.Errorf("error parsing script options: %w", err)
			}
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&b.Options); err != nil {
				if uerr := json.Unmarshal(data, &b.Options); uerr != nil {
					return uerr
				}
				logger.WithError(err).Warn("There were unknown fields in the options exported in the script")
			}
		case consts.SetupFn:
			return errors.New("exported 'setup' must be a function")
		case consts.TeardownFn:
			return errors.New("exported 'teardown' must be a function")
		}
	}

	if len(b.callableExports) == 0 {
		return errors.New("no exported functions in script")
	}

	return nil
}

// Instantiate creates a new runtime from this bundle.
func (b *Bundle) Instantiate(ctx context.Context, vuID uint64) (*BundleInstance, error) {
	// Instantiate the bundle into a new VM using a bound init context. This uses a context with a
	// runtime, but no state, to allow module-provided types to function within the init context.
	vuImpl := &moduleVUImpl{ctx: ctx, runtime: goja.New()}
	vuImpl.eventLoop = eventloop.New(vuImpl)
	instance, err := b.instantiate(vuImpl, vuID)
	if err != nil {
		return nil, err
	}

	bi := &BundleInstance{
		Runtime:            vuImpl.runtime,
		env:                b.preInitState.RuntimeOptions.Env,
		moduleVUImpl:       vuImpl,
		mainModuleInstance: instance,
	}

	// Grab any exported functions that could be executed. These were
	// already pre-validated in cmd.validateScenarioConfig(), just get them here.
	exports := instance.exports()

	jsOptions := exports.Get("options")
	var jsOptionsObj *goja.Object
	if jsOptions == nil || goja.IsNull(jsOptions) || goja.IsUndefined(jsOptions) {
		jsOptionsObj = vuImpl.runtime.NewObject()
		err := exports.Set("options", jsOptionsObj)
		if err != nil {
			return nil, fmt.Errorf("couldn't set exported options with merged values: %w", err)
		}
	} else {
		jsOptionsObj = jsOptions.ToObject(vuImpl.runtime)
	}

	var instErr error
	b.Options.ForEachSpecified("json", func(key string, val interface{}) {
		if err := jsOptionsObj.Set(key, val); err != nil {
			instErr = err
		}
	})

	return bi, instErr
}

func (b *Bundle) newCompiler(logger logrus.FieldLogger) *compiler.Compiler {
	c := compiler.New(logger)
	c.Options = compiler.Options{
		CompatibilityMode: b.CompatibilityMode,
		Strict:            true,
		SourceMapLoader:   generateSourceMapLoader(logger, b.filesystems),
	}
	return c
}

func (b *Bundle) instantiate(vuImpl *moduleVUImpl, vuID uint64) (moduleInstance, error) {
	rt := vuImpl.runtime
	err := b.setupJSRuntime(rt, int64(vuID), b.preInitState.Logger)
	if err != nil {
		return nil, err
	}

	initenv := &common.InitEnvironment{
		Logger:      b.preInitState.Logger,
		FileSystems: b.filesystems,
		CWD:         b.pwd,
		Registry:    b.preInitState.Registry,
		LookupEnv:   b.preInitState.LookupEnv,
	}

	modSys := newModuleSystem(b.moduleResolver, vuImpl)
	unbindInit := b.setInitGlobals(rt, modSys)
	vuImpl.initEnv = initenv
	defer func() {
		unbindInit()
		vuImpl.initEnv = nil
	}()

	// TODO: make something cleaner for interrupting scripts, and more unified
	// (e.g. as a part of the event loop or RunWithPanicCatching()?
	initDone := make(chan struct{})
	go func() {
		select {
		case <-vuImpl.ctx.Done():
			rt.Interrupt(vuImpl.ctx.Err())
		case initDone <- struct{}{}: // do nothing
		}
		close(initDone)
	}()

	var instance moduleInstance
	err = common.RunWithPanicCatching(b.preInitState.Logger, rt, func() error {
		return vuImpl.eventLoop.Start(func() error {
			//nolint:shadow,govet // here we shadow err on purpose
			mod, err := b.moduleResolver.resolve(b.pwd, b.sourceData.URL.String())
			if err != nil {
				return err // TODO wrap as this should never happen
			}
			instance = mod.Instantiate(vuImpl)
			return instance.execute()
		})
	})

	<-initDone

	if err != nil {
		var exception *goja.Exception
		if errors.As(err, &exception) {
			err = &scriptException{inner: exception}
		}
		return nil, err
	}
	if exports := instance.exports(); exports == nil {
		return nil, errors.New("exports must be an object")
	}

	// If we've already initialized the original VU init context, forbid
	// any subsequent VUs to open new files
	if vuID == 0 {
		allowOnlyOpenedFiles(b.filesystems["file"])
	}

	rt.SetRandSource(common.NewRandSource())

	return instance, nil
}

func (b *Bundle) setupJSRuntime(rt *goja.Runtime, vuID int64, logger logrus.FieldLogger) error {
	rt.SetFieldNameMapper(common.FieldNameMapper{})
	rt.SetRandSource(common.NewRandSource())

	env := make(map[string]string, len(b.preInitState.RuntimeOptions.Env))
	for key, value := range b.preInitState.RuntimeOptions.Env {
		env[key] = value
	}
	err := rt.Set("__ENV", env)
	if err != nil {
		return err
	}
	err = rt.Set("__VU", vuID)
	if err != nil {
		return err
	}
	err = rt.Set("console", newConsole(logger))
	if err != nil {
		return err
	}

	if b.CompatibilityMode == lib.CompatibilityModeExtended {
		err = rt.Set("global", rt.GlobalObject())
		if err != nil {
			return err
		}
	}
	return nil
}

func (b *Bundle) setInitGlobals(rt *goja.Runtime, modSys *moduleSystem) (unset func()) {
	mustSet := func(k string, v interface{}) {
		if err := rt.Set(k, v); err != nil {
			panic(fmt.Errorf("failed to set '%s' global object: %w", k, err))
		}
	}
	r := requireImpl{
		vu:      modSys.vu,
		modules: modSys,
		pwd:     b.pwd,
	}
	mustSet("require", r.require)

	mustSet("open", func(filename string, args ...string) (goja.Value, error) {
		if modSys.vu.State() != nil { // fix
			return nil, fmt.Errorf(cantBeUsedOutsideInitContextMsg, "open")
		}

		if filename == "" {
			return nil, errors.New("open() can't be used with an empty filename")
		}
		// This uses the pwd from the requireImpl
		return openImpl(rt, b.filesystems["file"], r.pwd, filename, args...)
	})
	return func() {
		mustSet("require", goja.Undefined())
		mustSet("open", goja.Undefined())
	}
}

func generateCJSLoad(b *Bundle, c *compiler.Compiler) cjsModuleLoader {
	return func(specifier *url.URL, name string) (*cjsModule, error) {
		if filepath.IsAbs(name) && runtime.GOOS == "windows" {
			b.preInitState.Logger.Warnf("'%s' was imported with an absolute path - this won't be cross-platform and "+
				"won't work if you move the script between machines or run it with `k6 cloud`; if absolute paths are "+
				"required, import them with the `file://` schema for slightly better compatibility",
				name)
		}
		d, err := loader.Load(b.preInitState.Logger, b.filesystems, specifier, name)
		if err != nil {
			return nil, err
		}
		return cjsmoduleFromString(specifier, d.Data, c)
	}
}
