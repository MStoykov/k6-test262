package test262

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
	"github.com/loadimpact/k6/js/compiler"
	"github.com/loadimpact/k6/lib"
	"github.com/loadimpact/k6/lib/testutils"
	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v2"
)

const (
	tc39BASE = "testdata/test262"
)

//noling:gochecknoglobals
var (
	invalidFormatError = errors.New("Invalid file format")

	// ignorableTestError = newSymbol(stringEmpty)

	sabStub = goja.MustCompile("sabStub.js", `
		Object.defineProperty(this, "SharedArrayBuffer", {
			get: function() {
				throw IgnorableTestError;
			}
		});`,
		false)

	esIdPrefixWhiteList = []string{
		"sec-array",
		"sec-%typedarray%",
		"sec-string",
		"sec-date",
		"sec-number",
		"sec-math",
		"sec-arraybuffer-length",
		"sec-arraybuffer",
		"sec-regexp",
	}
	featuresBlackList = []string{}
)

type tc39Test struct {
	name string
	f    func(t *testing.T)
}

type tc39BenchmarkItem struct {
	name     string
	duration time.Duration
}

type tc39BenchmarkData []tc39BenchmarkItem

type tc39TestCtx struct {
	compiler       *compiler.Compiler
	base           string
	t              *testing.T
	prgCache       map[string]*goja.Program
	prgCacheLock   sync.Mutex
	enableBench    bool
	benchmark      tc39BenchmarkData
	benchLock      sync.Mutex
	testQueue      []tc39Test
	expectedErrors map[string]string

	errorsLock sync.Mutex
	errors     map[string]string
}

type TC39MetaNegative struct {
	Phase, Type string
}

type tc39Meta struct {
	Negative TC39MetaNegative
	Includes []string
	Flags    []string
	Features []string
	Es5id    string
	Es6id    string
	Esid     string
}

func (m *tc39Meta) hasFlag(flag string) bool {
	for _, f := range m.Flags {
		if f == flag {
			return true
		}
	}
	return false
}

func parseTC39File(name string) (*tc39Meta, string, error) {
	f, err := os.Open(name) //nolint:gosec
	if err != nil {
		return nil, "", err
	}
	defer f.Close() //nolint:errcheck,gosec

	b, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, "", err
	}

	str := string(b)
	metaStart := strings.Index(str, "/*---")
	if metaStart == -1 {
		return nil, "", invalidFormatError
	}

	metaStart += 5
	metaEnd := strings.Index(str, "---*/")
	if metaEnd == -1 || metaEnd <= metaStart {
		return nil, "", invalidFormatError
	}

	var meta tc39Meta
	err = yaml.Unmarshal([]byte(str[metaStart:metaEnd]), &meta)
	if err != nil {
		return nil, "", err
	}

	if meta.Negative.Type != "" && meta.Negative.Phase == "" {
		return nil, "", errors.New("negative type is set, but phase isn't")
	}

	return &meta, str, nil
}

func (*tc39TestCtx) detachArrayBuffer(call goja.FunctionCall) goja.Value {
	if obj, ok := call.Argument(0).(*goja.Object); ok {
		if buf, ok := obj.Export().(*goja.ArrayBuffer); ok {
			buf.Detach()
			return goja.Undefined()
		}
	}
	panic(goja.New().NewTypeError("detachArrayBuffer() is called with incompatible argument"))
}

func (ctx *tc39TestCtx) fail(t testing.TB, name string, strict bool, errStr string) {
	nameKey := fmt.Sprintf("%s-strict:%v", name, strict)
	expected, ok := ctx.expectedErrors[nameKey]
	if ok {
		if !assert.Equal(t, expected, errStr) {
			ctx.errorsLock.Lock()
			fmt.Println("different")
			fmt.Println(expected)
			fmt.Println(errStr)
			ctx.errors[nameKey] = errStr
			ctx.errorsLock.Unlock()
		}
	} else {
		assert.Empty(t, errStr)
		ctx.errorsLock.Lock()
		fmt.Println("no error", name)
		ctx.errors[nameKey] = errStr
		ctx.errorsLock.Unlock()
	}
}

func (ctx *tc39TestCtx) runTC39Test(t testing.TB, name, src string, meta *tc39Meta, strict bool) {
	defer func() {
		if x := recover(); x != nil {
			panic(fmt.Sprintf("panic while running %s: %v", name, x))
		}
	}()
	vm := goja.New()
	_262 := vm.NewObject()
	ignorableTestError := vm.NewGoError(fmt.Errorf(""))
	vm.Set("IgnorableTestError", ignorableTestError)
	_ = _262.Set("detachArrayBuffer", ctx.detachArrayBuffer)
	_ = _262.Set("createRealm", func(goja.FunctionCall) goja.Value {
		panic(ignorableTestError)
	})
	vm.Set("$262", _262)
	vm.Set("print", t.Log)
	_, err := vm.RunProgram(sabStub)
	if err != nil {
		panic(err)
	}
	if strict {
		src = "'use strict';\n" + src
	}
	early, err := ctx.runTC39Script(name, src, meta.Includes, vm)
	failf := func(str string, args ...interface{}) {
		str = fmt.Sprintf(str, args)
		ctx.fail(t, name, strict, str)
	}

	if err != nil {
		if meta.Negative.Type == "" {
			if err, ok := err.(*goja.Exception); ok {
				if err.Value() == ignorableTestError {
					t.Skip("Test threw IgnorableTestError")
				}
			}
			failf("%s: %v", name, err)
			return
		} else {
			if meta.Negative.Phase == "early" && !early || meta.Negative.Phase == "runtime" && early {
				failf("%s: error %v happened at the wrong phase (expected %s)", name, err, meta.Negative.Phase)
				return
			}
			var errType string

			switch err := err.(type) {
			case *goja.Exception:
				if o, ok := err.Value().(*goja.Object); ok {
					if c := o.Get("constructor"); c != nil {
						if c, ok := c.(*goja.Object); ok {
							errType = c.Get("name").String()
						} else {
							failf("%s: error constructor is not an object (%v)", name, o)
							return
						}
					} else {
						failf("%s: error does not have a constructor (%v)", name, o)
						return
					}
				} else {
					failf("%s: error is not an object (%v)", name, err.Value())
					return
				}
			case *goja.CompilerSyntaxError, *parser.Error, parser.ErrorList:
				errType = "SyntaxError"
			case *goja.CompilerReferenceError:
				errType = "ReferenceError"
			default:
				failf("%s: error is not a JS error: %v", name, err)
				return
			}

			_ = errType
			if errType != meta.Negative.Type {
				// vm.vm.prg.dumpCode(t.Logf)
				failf("%s: unexpected error type (%s), expected (%s)", name, errType, meta.Negative.Type)
				return
			}
		}
	} else {
		if meta.Negative.Type != "" {
			// vm.vm.prg.dumpCode(t.Logf)
			failf("%s: Expected error: %v", name, err)
			return
		}
	}

	/*
		if vm.vm.sp != 0 {
			t.Fatalf("sp: %d", vm.vm.sp)
		}

		if l := len(vm.vm.iterStack); l > 0 {
			t.Fatalf("iter stack is not empty: %d", l)
		}
	*/
}

func (ctx *tc39TestCtx) runTC39File(name string, t testing.TB) {
	p := path.Join(ctx.base, name)
	meta, src, err := parseTC39File(p)
	if err != nil {
		// t.Fatalf("Could not parse %s: %v", name, err)
		t.Errorf("Could not parse %s: %v", name, err)
		return
	}
	// if meta.Es6id == "" && meta.Es5id == "" {
	if meta.Es6id == "" && meta.Es5id == "" {
		skip := true
		/*
			// t.Logf("%s: Not ES5, skipped", name)
			if es6WhiteList[name] {
				skip = false
			} else {
				if meta.Es6id != "" {
					for _, prefix := range es6IdWhiteList {
						if strings.HasPrefix(meta.Es6id, prefix) &&
							(len(meta.Es6id) == len(prefix) || meta.Es6id[len(prefix)] == '.') {

							skip = false
							break
						}
					}
				}
			}
			if skip {
				if meta.Esid != "" {
					for _, prefix := range esIdPrefixWhiteList {
						if strings.HasPrefix(meta.Esid, prefix) &&
							(len(meta.Esid) == len(prefix) || meta.Esid[len(prefix)] == '.') {

							skip = false
							break
						}
					}
				}
		*/
		if skip {
			t.Skipf("Not ES6 or ES5 esid: %s", meta.Esid)
		}

		for _, feature := range meta.Features {
			for _, bl := range featuresBlackList {
				if feature == bl {
					t.Skip("Blacklisted feature")
				}
			}
		}
	}

	var startTime time.Time
	if ctx.enableBench {
		startTime = time.Now()
	}

	hasRaw := meta.hasFlag("raw")

	if hasRaw || !meta.hasFlag("onlyStrict") {
		// log.Printf("Running normal test: %s", name)
		// t.Logf("Running normal test: %s", name)
		ctx.runTC39Test(t, name, src, meta, false)
	}

	if !hasRaw && !meta.hasFlag("noStrict") {
		// log.Printf("Running strict test: %s", name)
		// t.Logf("Running strict test: %s", name)
		ctx.runTC39Test(t, name, src, meta, true)
	}

	if ctx.enableBench {
		ctx.benchLock.Lock()
		ctx.benchmark = append(ctx.benchmark, tc39BenchmarkItem{
			name:     name,
			duration: time.Since(startTime),
		})
		ctx.benchLock.Unlock()
	}
}

func (ctx *tc39TestCtx) init() {
	ctx.prgCache = make(map[string]*goja.Program)
	ctx.errors = make(map[string]string)

	b, err := ioutil.ReadFile("./breaking_test_errors.json")
	if err != nil {
		panic(err)
	}
	ctx.expectedErrors = make(map[string]string, 1000)
	err = json.Unmarshal(b, &ctx.expectedErrors)
	if err != nil {
		panic(err)
	}
}

func (ctx *tc39TestCtx) compile(base, name string) (*goja.Program, error) {
	ctx.prgCacheLock.Lock()
	defer ctx.prgCacheLock.Unlock()

	prg := ctx.prgCache[name]
	if prg == nil {
		fname := path.Join(base, name)
		f, err := os.Open(fname) //nolint:gosec
		if err != nil {
			return nil, err
		}
		defer f.Close() //nolint:gosec,errcheck

		b, err := ioutil.ReadAll(f)
		if err != nil {
			return nil, err
		}

		str := string(b)
		prg, _, err = ctx.compiler.Compile(str, name, "", "", false, lib.CompatibilityModeExtended)
		if err != nil {
			return nil, err
		}
		ctx.prgCache[name] = prg
	}

	return prg, nil
}

func (ctx *tc39TestCtx) runFile(base, name string, vm *goja.Runtime) error {
	prg, err := ctx.compile(base, name)
	if err != nil {
		return err
	}
	_, err = vm.RunProgram(prg)
	return err
}

func (ctx *tc39TestCtx) runTC39Script(name, src string, includes []string, vm *goja.Runtime) (early bool, err error) {
	early = true
	err = ctx.runFile(ctx.base, path.Join("harness", "assert.js"), vm)
	if err != nil {
		return
	}

	err = ctx.runFile(ctx.base, path.Join("harness", "sta.js"), vm)
	if err != nil {
		return
	}

	for _, include := range includes {
		err = ctx.runFile(ctx.base, path.Join("harness", include), vm)
		if err != nil {
			return
		}
	}

	var p *goja.Program
	p, _, err = ctx.compiler.Compile(src, name, "", "", false, lib.CompatibilityModeExtended)

	if err != nil {
		return
	}

	early = false
	_, err = vm.RunProgram(p)

	return
}

func (ctx *tc39TestCtx) runTC39Tests(name string) {
	files, err := ioutil.ReadDir(path.Join(ctx.base, name))
	if err != nil {
		ctx.t.Fatal(err)
	}

	for _, file := range files {
		if file.Name()[0] == '.' {
			continue
		}
		if file.IsDir() {
			ctx.runTC39Tests(path.Join(name, file.Name()))
		} else {
			if strings.HasSuffix(file.Name(), ".js") && !strings.HasSuffix(file.Name(), "_FIXTURE.js") {
				name := path.Join(name, file.Name())
				ctx.runTest(name, func(t *testing.T) {
					ctx.runTC39File(name, t)
				})
			}
		}
	}
}

func TestTC39(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	if _, err := os.Stat(tc39BASE); err != nil {
		t.Skipf("If you want to run tc39 tests, download them from https://github.com/tc39/test262 and put into %s. The last working commit is 1ba3a7c4a93fc93b3d0d7e4146f59934a896837d. (%v)", tc39BASE, err)
	}

	ctx := &tc39TestCtx{
		base:     tc39BASE,
		compiler: compiler.New(testutils.NewLogger(t)),
	}
	ctx.init()
	// ctx.enableBench = true

	t.Run("tc39", func(t *testing.T) {
		ctx.t = t
		ctx.runTC39Tests("test")
		/*
			// ctx.runTC39File("test/language/types/number/8.5.1.js", t)
			// ctx.runTC39Tests("test/language")
			ctx.runTC39Tests("test/language/expressions")
			ctx.runTC39Tests("test/language/arguments-object")
			ctx.runTC39Tests("test/language/asi")
			ctx.runTC39Tests("test/language/directive-prologue")
			ctx.runTC39Tests("test/language/function-code")
			ctx.runTC39Tests("test/language/eval-code")
			ctx.runTC39Tests("test/language/global-code")
			ctx.runTC39Tests("test/language/identifier-resolution")
			ctx.runTC39Tests("test/language/identifiers")
			// ctx.runTC39Tests("test/language/literals") // octal sequences in strict mode
			ctx.runTC39Tests("test/language/punctuators")
			ctx.runTC39Tests("test/language/reserved-words")
			ctx.runTC39Tests("test/language/source-text")
			ctx.runTC39Tests("test/language/statements")
			ctx.runTC39Tests("test/language/types")
			ctx.runTC39Tests("test/language/white-space")
			ctx.runTC39Tests("test/built-ins")
			ctx.runTC39Tests("test/annexB/built-ins/String/prototype/substr")
			ctx.runTC39Tests("test/annexB/built-ins/escape")
			ctx.runTC39Tests("test/annexB/built-ins/unescape")
			ctx.runTC39Tests("test/annexB/built-ins/RegExp")
		*/

		ctx.flush()
	})

	if ctx.enableBench {
		sort.Slice(ctx.benchmark, func(i, j int) bool {
			return ctx.benchmark[i].duration > ctx.benchmark[j].duration
		})
		bench := ctx.benchmark
		if len(bench) > 50 {
			bench = bench[:50]
		}
		for _, item := range bench {
			fmt.Printf("%s\t%d\n", item.name, item.duration/time.Millisecond)
		}
	}
	if len(ctx.errors) > 0 {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(ctx.errors)
	}
}
