package interp

import (
	"bytes"
	"context"
	"crypto/md5"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"math/big"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mitchellh/mapstructure"
	"github.com/wader/fq/internal/ansi"
	"github.com/wader/fq/internal/bitioextra"
	"github.com/wader/fq/internal/colorjson"
	"github.com/wader/fq/internal/ctxstack"
	"github.com/wader/fq/internal/gojqextra"
	"github.com/wader/fq/internal/ioextra"
	"github.com/wader/fq/internal/mathextra"
	"github.com/wader/fq/internal/pos"
	"github.com/wader/fq/pkg/bitio"
	"github.com/wader/fq/pkg/decode"
	"github.com/wader/fq/pkg/registry"

	"github.com/wader/gojq"
)

//go:embed interp.jq
//go:embed internal.jq
//go:embed eval.jq
//go:embed options.jq
//go:embed ansi.jq
//go:embed binary.jq
//go:embed decode.jq
//go:embed funcs.jq
//go:embed grep.jq
//go:embed args.jq
//go:embed query.jq
//go:embed repl.jq
//go:embed help.jq
//go:embed format_decode.jq
//go:embed format_func.jq
//go:embed format_include.jq
var builtinFS embed.FS

var initSource = `include "@builtin/interp";`

var functionRegisterFns []func(i *Interp) []Function

func init() {
	functionRegisterFns = append(functionRegisterFns, func(i *Interp) []Function {
		return []Function{
			{"_readline", 0, 1, nil, i._readline},
			{"_eval", 1, 2, nil, i._eval},
			{"_stdin", 0, 1, nil, i.makeStdioFn("stdin", i.os.Stdin())},
			{"_stdout", 0, 0, nil, i.makeStdioFn("stdout", i.os.Stdout())},
			{"_stderr", 0, 0, nil, i.makeStdioFn("stderr", i.os.Stderr())},
			{"_extkeys", 0, 0, i._extKeys, nil},
			{"_exttype", 0, 0, i._extType, nil},
			{"_global_state", 0, 1, i.makeStateFn(i.state), nil},
			{"history", 0, 0, i.history, nil},
			{"_display", 1, 1, nil, i._display},
			{"_can_display", 0, 0, i._canDisplay, nil},
			{"_print_color_json", 0, 1, nil, i._printColorJSON},
			{"_is_completing", 0, 1, i._isCompleting, nil},
		}
	})
}

type valueError struct {
	v interface{}
}

func (v valueError) Error() string      { return fmt.Sprintf("error: %v", v.v) }
func (v valueError) Value() interface{} { return v.v }

type compileError struct {
	err      error
	what     string
	filename string
	pos      pos.Pos
}

func (ce compileError) Value() interface{} {
	return map[string]interface{}{
		"error":    ce.err.Error(),
		"what":     ce.what,
		"filename": ce.filename,
		"line":     ce.pos.Line,
		"column":   ce.pos.Column,
	}
}
func (ce compileError) Error() string {
	filename := ce.filename
	if filename == "" {
		filename = "expr"
	}
	return fmt.Sprintf("%s:%d:%d: %s: %s", filename, ce.pos.Line, ce.pos.Column, ce.what, ce.err.Error())
}

var ErrEOF = io.EOF
var ErrInterrupt = errors.New("Interrupt")

// gojq errors can implement this to signal exit code
type Exiter interface {
	ExitCode() int
}

// gojq halt_error uses this
type IsEmptyErrorer interface {
	IsEmptyError() bool
}

type Terminal interface {
	Size() (int, int)
	IsTerminal() bool
}

type Input interface {
	fs.File
	Terminal
}

type Output interface {
	io.Writer
	Terminal
}

type Platform struct {
	OS   string
	Arch string
}

type ReadlineOpts struct {
	Prompt     string
	CompleteFn func(line string, pos int) (newLine []string, shared int)
}

type OS interface {
	Platform() Platform
	Stdin() Input
	Stdout() Output
	Stderr() Output
	InterruptChan() chan struct{}
	Args() []string
	Environ() []string
	ConfigDir() (string, error)
	// FS.File returned by FS().Open() can optionally implement io.Seeker
	FS() fs.FS
	Readline(opts ReadlineOpts) (string, error)
	History() ([]string, error)
}

type FixedFileInfo struct {
	FName    string
	FSize    int64
	FMode    fs.FileMode
	FModTime time.Time
	FIsDir   bool
	FSys     interface{}
}

func (ffi FixedFileInfo) Name() string       { return ffi.FName }
func (ffi FixedFileInfo) Size() int64        { return ffi.FSize }
func (ffi FixedFileInfo) Mode() fs.FileMode  { return ffi.FMode }
func (ffi FixedFileInfo) ModTime() time.Time { return ffi.FModTime }
func (ffi FixedFileInfo) IsDir() bool        { return ffi.FIsDir }
func (ffi FixedFileInfo) Sys() interface{}   { return ffi.FSys }

type FileReader struct {
	R        io.Reader
	FileInfo FixedFileInfo
}

func (rf FileReader) Stat() (fs.FileInfo, error) { return rf.FileInfo, nil }
func (rf FileReader) Read(p []byte) (int, error) { return rf.R.Read(p) }
func (FileReader) Close() error                  { return nil }

type Value interface {
	gojq.JQValue

	ExtType() string
	ExtKeys() []string
}

type Display interface {
	Display(w io.Writer, opts Options) error
}

type JQValueEx interface {
	JQValueToGoJQEx(optsFn func() Options) interface{}
}

func valuePath(v *decode.Value) []interface{} {
	var parts []interface{}

	for v.Parent != nil {
		switch vv := v.Parent.V.(type) {
		case *decode.Compound:
			if vv.IsArray {
				parts = append([]interface{}{v.Index}, parts...)
			} else {
				parts = append([]interface{}{v.Name}, parts...)
			}
		}
		v = v.Parent
	}

	return parts
}

func valuePathExprDecorated(v *decode.Value, d Decorator) string {
	parts := []string{"."}

	for i, p := range valuePath(v) {
		switch p := p.(type) {
		case string:
			if i > 0 {
				parts = append(parts, ".")
			}
			parts = append(parts, d.ObjectKey.Wrap(p))
		case int:
			indexStr := strconv.Itoa(p)
			parts = append(parts, fmt.Sprintf("%s%s%s", d.Index.F("["), d.Number.F(indexStr), d.Index.F("]")))
		}
	}

	return strings.Join(parts, "")
}

type iterFn func() (interface{}, bool)

func (i iterFn) Next() (interface{}, bool) { return i() }

type loadModule struct {
	init func() ([]*gojq.Query, error)
	load func(name string) (*gojq.Query, error)
}

func (l loadModule) LoadInitModules() ([]*gojq.Query, error)     { return l.init() }
func (l loadModule) LoadModule(name string) (*gojq.Query, error) { return l.load(name) }

func toString(v interface{}) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case gojq.JQValue:
		return toString(v.JQValueToGoJQ())
	default:
		b, err := toBytes(v)
		if err != nil {
			return "", fmt.Errorf("value can't be a string")
		}

		return string(b), nil
	}
}

func toBigInt(v interface{}) (*big.Int, error) {
	switch v := v.(type) {
	case int:
		return new(big.Int).SetInt64(int64(v)), nil
	case float64:
		return new(big.Int).SetInt64(int64(v)), nil
	case *big.Int:
		return v, nil
	default:
		return nil, fmt.Errorf("value is not a number")
	}
}

func toBytes(v interface{}) ([]byte, error) {
	switch v := v.(type) {
	default:
		br, err := toBitReader(v)
		if err != nil {
			return nil, fmt.Errorf("value is not bytes")
		}
		buf := &bytes.Buffer{}
		if _, err := bitioextra.CopyBits(buf, br); err != nil {
			return nil, err
		}

		return buf.Bytes(), nil
	}
}

func queryErrorPosition(expr string, v error) pos.Pos {
	var offset int

	if tokIf, ok := v.(interface{ Token() (string, int) }); ok { //nolint:errorlint
		_, offset = tokIf.Token()
	}
	if offset >= 0 {
		return pos.NewFromOffset(expr, offset)
	}
	return pos.Pos{}
}

type Variable struct {
	Name  string
	Value interface{}
}

type Function struct {
	Name     string
	MinArity int
	MaxArity int
	Fn       func(interface{}, []interface{}) interface{}
	IterFn   func(interface{}, []interface{}) gojq.Iter
}

type RunMode int

const (
	ScriptMode RunMode = iota
	REPLMode
	CompletionMode
)

type evalInstance struct {
	ctx          context.Context
	output       io.Writer
	isCompleting bool
	includeSeen  map[string]struct{}
}

type Interp struct {
	registry       *registry.Registry
	os             OS
	initQuery      *gojq.Query
	includeCache   map[string]*gojq.Query
	interruptStack *ctxstack.Stack
	// global state, is ref as Interp is cloned per eval
	state *interface{}

	// new for each eval, other values are copied by value
	evalInstance evalInstance
}

func New(os OS, registry *registry.Registry) (*Interp, error) {
	var err error

	i := &Interp{
		os:       os,
		registry: registry,
	}

	i.includeCache = map[string]*gojq.Query{}
	i.initQuery, err = gojq.Parse(initSource)
	if err != nil {
		return nil, fmt.Errorf("init:%s: %w", queryErrorPosition(initSource, err), err)
	}
	// TODO: refactor ctxstack have a CancelTop and return c context to Stop?
	i.interruptStack = ctxstack.New(func(stopCh chan struct{}) {
		select {
		case <-stopCh:
			return
		case <-os.InterruptChan():
			return
		}
	})
	i.state = new(interface{})

	return i, nil
}

func (i *Interp) Stop() {
	// TODO: cancel all run instances?
	i.interruptStack.Stop()
}

func (i *Interp) Main(ctx context.Context, output Output, versionStr string) error {
	var args []interface{}
	for _, a := range i.os.Args() {
		args = append(args, a)
	}

	platform := i.os.Platform()
	input := map[string]interface{}{
		"args":    args,
		"version": versionStr,
		"os":      platform.OS,
		"arch":    platform.Arch,
	}

	iter, err := i.EvalFunc(ctx, input, "_main", nil, EvalOpts{output: output})
	if err != nil {
		fmt.Fprintln(i.os.Stderr(), err)
		return err
	}
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}

		switch v := v.(type) {
		case error:
			if emptyErr, ok := v.(IsEmptyErrorer); ok && emptyErr.IsEmptyError() { //nolint:errorlint
				// no output
			} else if errors.Is(v, context.Canceled) {
				// ignore context cancel here for now, which means user somehow interrupted the interpreter
				// TODO: handle this inside interp.jq instead but then we probably have to do nested
				// eval and or also use different contexts for the interpreter and reading/decoding
			} else {
				fmt.Fprintln(i.os.Stderr(), v)
			}
			return v
		case [2]interface{}:
			fmt.Fprintln(i.os.Stderr(), v[:]...)
		default:
			// TODO: can this happen?
			fmt.Fprintln(i.os.Stderr(), v)
		}
	}

	return nil
}

func (i *Interp) _readline(c interface{}, a []interface{}) gojq.Iter {
	if i.evalInstance.isCompleting {
		return gojq.NewIter()
	}

	var opts struct {
		Promopt  string  `mapstructure:"prompt"`
		Complete string  `mapstructure:"complete"`
		Timeout  float64 `mapstructure:"timeout"`
	}

	if len(a) > 0 {
		_ = mapstructure.Decode(a[0], &opts)
	}

	expr, err := i.os.Readline(ReadlineOpts{
		Prompt: opts.Promopt,
		CompleteFn: func(line string, pos int) (newLine []string, shared int) {
			completeCtx := i.evalInstance.ctx
			if opts.Timeout > 0 {
				var completeCtxCancelFn context.CancelFunc
				completeCtx, completeCtxCancelFn = context.WithTimeout(i.evalInstance.ctx, time.Duration(opts.Timeout*float64(time.Second)))
				defer completeCtxCancelFn()
			}

			names, shared, err := func() (newLine []string, shared int, err error) {
				// c | opts.Complete(line; pos)
				vs, err := i.EvalFuncValues(
					completeCtx,
					c,
					opts.Complete,
					[]interface{}{line, pos},
					EvalOpts{
						output:       ioextra.DiscardCtxWriter{Ctx: completeCtx},
						isCompleting: true,
					},
				)
				if err != nil {
					return nil, pos, err
				}
				if len(vs) < 1 {
					return nil, pos, fmt.Errorf("no values")
				}
				v := vs[0]
				if vErr, ok := v.(error); ok {
					return nil, pos, vErr
				}

				// {abc: 123, abd: 123} | complete(".ab"; 3) will return {prefix: "ab", names: ["abc", "abd"]}

				var result struct {
					Names  []string `mapstructure:"names"`
					Prefix string   `mapstructure:"prefix"`
				}

				_ = mapstructure.Decode(v, &result)
				if len(result.Names) == 0 {
					return nil, pos, nil
				}

				sharedLen := len(result.Prefix)

				return result.Names, sharedLen, nil
			}()

			// TODO: how to report err?
			_ = err

			return names, shared
		},
	})

	if errors.Is(err, ErrInterrupt) {
		return gojq.NewIter(valueError{"interrupt"})
	} else if errors.Is(err, ErrEOF) {
		return gojq.NewIter(valueError{"eof"})
	} else if err != nil {
		return gojq.NewIter(err)
	}

	return gojq.NewIter(expr)
}

func (i *Interp) _eval(c interface{}, a []interface{}) gojq.Iter {
	var err error
	expr, err := toString(a[0])
	if err != nil {
		return gojq.NewIter(fmt.Errorf("expr: %w", err))
	}
	var filenameHint string
	if len(a) >= 2 {
		filenameHint, err = toString(a[1])
		if err != nil {
			return gojq.NewIter(fmt.Errorf("filename hint: %w", err))
		}
	}

	iter, err := i.Eval(i.evalInstance.ctx, c, expr, EvalOpts{
		filename: filenameHint,
		output:   i.evalInstance.output,
	})
	if err != nil {
		return gojq.NewIter(err)
	}

	return iter
}

func (i *Interp) _extKeys(c interface{}, a []interface{}) interface{} {
	if v, ok := c.(Value); ok {
		var vs []interface{}
		for _, s := range v.ExtKeys() {
			vs = append(vs, s)
		}
		return vs
	}
	return nil
}

func (i *Interp) _extType(c interface{}, a []interface{}) interface{} {
	if v, ok := c.(Value); ok {
		return v.ExtType()
	}
	return gojqextra.Typeof(c)
}

func (i *Interp) makeStateFn(state *interface{}) func(c interface{}, a []interface{}) interface{} {
	return func(c interface{}, a []interface{}) interface{} {
		if len(a) > 0 {
			*state = a[0]
		}
		return *state
	}
}

func (i *Interp) makeStdioFn(name string, t Terminal) func(c interface{}, a []interface{}) gojq.Iter {
	return func(c interface{}, a []interface{}) gojq.Iter {
		switch {
		case len(a) == 1:
			if i.evalInstance.isCompleting {
				return gojq.NewIter("")
			}

			r, ok := t.(io.Reader)
			if !ok {
				return gojq.NewIter(fmt.Errorf("%s is not readable", name))
			}
			l, ok := gojqextra.ToInt(a[0])
			if !ok {
				return gojq.NewIter(gojqextra.FuncTypeError{Name: name, V: a[0]})
			}

			buf := make([]byte, l)
			n, err := io.ReadFull(r, buf)
			s := string(buf[0:n])

			vs := []interface{}{s}
			switch {
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				vs = append(vs, valueError{"eof"})
			default:
				vs = append(vs, err)
			}

			return gojq.NewIter(vs...)
		case c == nil:
			w, h := t.Size()
			return gojq.NewIter(map[string]interface{}{
				"is_terminal": t.IsTerminal(),
				"width":       w,
				"height":      h,
			})
		default:
			if i.evalInstance.isCompleting {
				return gojq.NewIter()
			}

			w, ok := t.(io.Writer)
			if !ok {
				return gojq.NewIter(fmt.Errorf("%v: it not writeable", c))
			}
			if _, err := fmt.Fprint(w, c); err != nil {
				return gojq.NewIter(err)
			}
			return gojq.NewIter()
		}
	}
}

func (i *Interp) history(c interface{}, a []interface{}) interface{} {
	hs, err := i.os.History()
	if err != nil {
		return err
	}
	var vs []interface{}
	for _, s := range hs {
		vs = append(vs, s)
	}
	return vs
}

func (i *Interp) _display(c interface{}, a []interface{}) gojq.Iter {
	opts := i.Options(a[0])

	switch v := c.(type) {
	case Display:
		if err := v.Display(i.evalInstance.output, opts); err != nil {
			return gojq.NewIter(err)
		}
		return gojq.NewIter()
	default:
		return gojq.NewIter(fmt.Errorf("%+#v: not displayable", c))
	}
}

func (i *Interp) _canDisplay(c interface{}, a []interface{}) interface{} {
	_, ok := c.(Display)
	return ok
}

func (i *Interp) _printColorJSON(c interface{}, a []interface{}) gojq.Iter {
	opts := i.Options(a[0])

	cj, err := i.NewColorJSON(opts)
	if err != nil {
		return gojq.NewIter(err)
	}
	if err := cj.Marshal(c, i.evalInstance.output); err != nil {
		return gojq.NewIter(err)
	}

	return gojq.NewIter()
}

func (i *Interp) _isCompleting(c interface{}, a []interface{}) interface{} {
	return i.evalInstance.isCompleting
}

type pathResolver struct {
	prefix string
	open   func(filename string) (io.ReadCloser, string, error)
}

func (i *Interp) lookupPathResolver(filename string) (pathResolver, error) {
	configDir, err := i.os.ConfigDir()
	if err != nil {
		return pathResolver{}, err
	}

	resolvePaths := []pathResolver{
		{
			"@builtin/",
			func(filename string) (io.ReadCloser, string, error) {
				f, err := builtinFS.Open(filename)
				return f, "@builtin/" + filename, err
			},
		},
		{
			"@config/", func(filename string) (io.ReadCloser, string, error) {
				p := path.Join(configDir, filename)
				f, err := i.os.FS().Open(p)
				return f, p, err
			},
		},
		{
			"", func(filename string) (io.ReadCloser, string, error) {
				if path.IsAbs(filename) {
					f, err := i.os.FS().Open(filename)
					return f, filename, err
				}

				// TODO: jq $ORIGIN
				for _, includePath := range append([]string{"./"}, i.includePaths()...) {
					p := path.Join(includePath, filename)
					if f, err := i.os.FS().Open(path.Join(includePath, filename)); err == nil {
						return f, p, nil
					}
				}

				return nil, "", &fs.PathError{Op: "open", Path: filename, Err: fs.ErrNotExist}
			},
		},
	}
	for _, p := range resolvePaths {
		if strings.HasPrefix(filename, p.prefix) {
			return p, nil
		}
	}
	return pathResolver{}, fmt.Errorf("could not resolve path: %s", filename)
}

type EvalOpts struct {
	filename     string
	output       io.Writer
	isCompleting bool
}

func (i *Interp) Eval(ctx context.Context, c interface{}, expr string, opts EvalOpts) (gojq.Iter, error) {
	gq, err := gojq.Parse(expr)
	if err != nil {
		p := queryErrorPosition(expr, err)
		return nil, compileError{
			err:      err,
			what:     "parse",
			filename: opts.filename,
			pos:      p,
		}
	}

	// make copy of interp and give it its own eval context
	ci := *i
	ni := &ci
	ni.evalInstance = evalInstance{
		includeSeen: map[string]struct{}{},
	}

	var variableNames []string
	var variableValues []interface{}
	for k, v := range i.slurps() {
		variableNames = append(variableNames, "$"+k)
		variableValues = append(variableValues, v)
	}

	var funcCompilerOpts []gojq.CompilerOption
	for _, frFn := range functionRegisterFns {
		for _, f := range frFn(ni) {
			if f.IterFn != nil {
				funcCompilerOpts = append(funcCompilerOpts,
					gojq.WithIterFunction(f.Name, f.MinArity, f.MaxArity, f.IterFn))
			} else {
				funcCompilerOpts = append(funcCompilerOpts,
					gojq.WithFunction(f.Name, f.MinArity, f.MaxArity, f.Fn))
			}
		}
	}

	compilerOpts := append([]gojq.CompilerOption{}, funcCompilerOpts...)
	compilerOpts = append(compilerOpts, gojq.WithEnvironLoader(ni.os.Environ))
	compilerOpts = append(compilerOpts, gojq.WithVariables(variableNames))
	compilerOpts = append(compilerOpts, gojq.WithModuleLoader(loadModule{
		init: func() ([]*gojq.Query, error) {
			return []*gojq.Query{i.initQuery}, nil
		},
		load: func(name string) (*gojq.Query, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			var filename string
			// support include "nonexisting?" to ignore include error
			var isTry bool
			if strings.HasSuffix(name, "?") {
				isTry = true
				filename = name[0 : len(name)-1]
			} else {
				filename = name
			}
			filename = filename + ".jq"

			pr, err := i.lookupPathResolver(filename)
			if err != nil {
				return nil, err
			}

			// skip if this eval instance has already included the file
			if _, ok := ni.evalInstance.includeSeen[filename]; ok {
				return &gojq.Query{Term: &gojq.Term{Type: gojq.TermTypeIdentity}}, nil
			}
			ni.evalInstance.includeSeen[filename] = struct{}{}

			// return cached version if file has already been parsed
			if q, ok := ni.includeCache[filename]; ok {
				return q, nil
			}

			filenamePart := strings.TrimPrefix(filename, pr.prefix)

			f, absPath, err := pr.open(filenamePart)
			if err != nil {
				if !isTry {
					return nil, err
				}
				f = io.NopCloser(&bytes.Buffer{})
			}
			defer f.Close()

			b, err := io.ReadAll(f)
			if err != nil {
				return nil, err
			}
			s := string(b)
			q, err := gojq.Parse(s)
			if err != nil {
				p := queryErrorPosition(s, err)
				return nil, compileError{
					err:      err,
					what:     "parse",
					filename: absPath,
					pos:      p,
				}
			}

			// not identity body means it returns something, threat as dynamic include
			if q.Term == nil || q.Term.Type != gojq.TermTypeIdentity {
				gc, err := gojq.Compile(q, funcCompilerOpts...)
				if err != nil {
					return nil, err
				}
				iter := gc.RunWithContext(context.Background(), nil)
				var vs []interface{}
				for {
					v, ok := iter.Next()
					if !ok {
						break
					}
					if err, ok := v.(error); ok {
						return nil, err
					}
					vs = append(vs, v)
				}
				if len(vs) != 1 {
					return nil, fmt.Errorf("dynamic include: must output one string, got: %#v", vs)
				}
				s, sOk := vs[0].(string)
				if !sOk {
					return nil, fmt.Errorf("dynamic include: must be string, got %#v", s)
				}
				q, err = gojq.Parse(s)
				if err != nil {
					p := queryErrorPosition(s, err)
					return nil, compileError{
						err:      err,
						what:     "dynamic include parse",
						filename: filenamePart,
						pos:      p,
					}
				}
			}

			// TODO: some better way of handling relative includes that
			// works with @builtin etc
			basePath := path.Dir(name)
			for _, qi := range q.Imports {
				rewritePath := func(base, includePath string) string {
					if strings.HasPrefix(includePath, "@") || path.IsAbs(includePath) {
						return includePath
					}

					return path.Join(base, includePath)
				}
				if qi.IncludePath != "" {
					qi.IncludePath = rewritePath(basePath, qi.IncludePath)
				}
				if qi.ImportPath != "" {
					qi.ImportPath = rewritePath(basePath, qi.ImportPath)
				}
			}

			i.includeCache[filename] = q

			return q, nil
		},
	}))

	gc, err := gojq.Compile(gq, compilerOpts...)
	if err != nil {
		p := queryErrorPosition(expr, err)
		return nil, compileError{
			err:      err,
			what:     "compile",
			filename: opts.filename,
			pos:      p,
		}
	}

	output := opts.output
	if opts.output == nil {
		output = ioutil.Discard
	}

	runCtx, runCtxCancelFn := i.interruptStack.Push(ctx)
	ni.evalInstance.ctx = runCtx
	ni.evalInstance.output = ioextra.CtxWriter{Writer: output, Ctx: runCtx}
	// inherit or maybe set
	ni.evalInstance.isCompleting = i.evalInstance.isCompleting || opts.isCompleting
	iter := gc.RunWithContext(runCtx, c, variableValues...)

	iterWrapper := iterFn(func() (interface{}, bool) {
		v, ok := iter.Next()
		// gojq ctx cancel will not return ok=false, just cancelled error
		if !ok {
			runCtxCancelFn()
		} else if _, ok := v.(error); ok {
			runCtxCancelFn()
		}
		return v, ok
	})

	return iterWrapper, nil
}

func (i *Interp) EvalFunc(ctx context.Context, c interface{}, name string, args []interface{}, opts EvalOpts) (gojq.Iter, error) {
	var argsExpr []string
	for i := range args {
		argsExpr = append(argsExpr, fmt.Sprintf("$_args[%d]", i))
	}
	argExpr := ""
	if len(argsExpr) > 0 {
		argExpr = "(" + strings.Join(argsExpr, ";") + ")"
	}

	trampolineInput := map[string]interface{}{
		"input": c,
		"args":  args,
	}
	// _args to mark variable as internal and hide it from completion
	// {input: ..., args: [...]} | .args as {args: $_args} | .input | name[($_args[0]; ...)]
	trampolineExpr := fmt.Sprintf(". as {args: $_args} | .input | %s%s", name, argExpr)
	iter, err := i.Eval(ctx, trampolineInput, trampolineExpr, opts)
	if err != nil {
		return nil, err
	}
	return iter, nil
}

func (i *Interp) EvalFuncValues(ctx context.Context, c interface{}, name string, args []interface{}, opts EvalOpts) ([]interface{}, error) {
	iter, err := i.EvalFunc(ctx, c, name, args, opts)
	if err != nil {
		return nil, err
	}

	var vs []interface{}
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		vs = append(vs, v)
	}

	return vs, nil
}

type Options struct {
	Depth          int               `mapstructure:"depth"`
	ArrayTruncate  int               `mapstructure:"array_truncate"`
	Verbose        bool              `mapstructure:"verbose"`
	Width          int               `mapstructure:"width"`
	DecodeProgress bool              `mapstructure:"decode_progress"`
	Color          bool              `mapstructure:"color"`
	Colors         map[string]string `mapstructure:"colors"`
	ByteColors     []struct {
		Ranges [][2]int `mapstructure:"ranges"`
		Value  string   `mapstructure:"value"`
	} `mapstructure:"byte_colors"`
	Unicode      bool   `mapstructure:"unicode"`
	RawOutput    bool   `mapstructure:"raw_output"`
	REPL         bool   `mapstructure:"repl"`
	RawString    bool   `mapstructure:"raw_string"`
	JoinString   string `mapstructure:"join_string"`
	Compact      bool   `mapstructure:"compact"`
	BitsFormat   string `mapstructure:"bits_format"`
	LineBytes    int    `mapstructure:"line_bytes"`
	DisplayBytes int    `mapstructure:"display_bytes"`
	AddrBase     int    `mapstructure:"addrbase"`
	SizeBase     int    `mapstructure:"sizebase"`

	Decorator    Decorator
	BitsFormatFn func(br bitio.ReaderAtSeeker) (interface{}, error)
}

func bitsFormatFnFromOptions(opts Options) func(br bitio.ReaderAtSeeker) (interface{}, error) {
	switch opts.BitsFormat {
	case "md5":
		return func(br bitio.ReaderAtSeeker) (interface{}, error) {
			d := md5.New()
			if _, err := bitioextra.CopyBits(d, br); err != nil {
				return "", err
			}
			return hex.EncodeToString(d.Sum(nil)), nil
		}
	case "base64":
		return func(br bitio.ReaderAtSeeker) (interface{}, error) {
			b := &bytes.Buffer{}
			e := base64.NewEncoder(base64.StdEncoding, b)
			if _, err := bitioextra.CopyBits(e, br); err != nil {
				return "", err
			}
			e.Close()
			return b.String(), nil
		}
	case "truncate":
		// TODO: configure
		return func(br bitio.ReaderAtSeeker) (interface{}, error) {
			b := &bytes.Buffer{}
			if _, err := bitioextra.CopyBits(b, bitio.NewLimitReader(br, 1024*8)); err != nil {
				return "", err
			}
			return b.String(), nil
		}
	case "string":
		return func(br bitio.ReaderAtSeeker) (interface{}, error) {
			b := &bytes.Buffer{}
			if _, err := bitioextra.CopyBits(b, br); err != nil {
				return "", err
			}
			return b.String(), nil
		}
	case "snippet":
		fallthrough
	default:
		return func(br bitio.ReaderAtSeeker) (interface{}, error) {
			b := &bytes.Buffer{}
			e := base64.NewEncoder(base64.StdEncoding, b)
			if _, err := bitioextra.CopyBits(e, bitio.NewLimitReader(br, 256*8)); err != nil {
				return "", err
			}
			e.Close()

			brLen, err := bitioextra.Len(br)
			if err != nil {
				return nil, err
			}

			return fmt.Sprintf("<%s>%s", mathextra.Bits(brLen).StringByteBits(opts.SizeBase), b.String()), nil
		}
	}
}

func (i *Interp) lookupState(key string) interface{} {
	if i.state == nil {
		return nil
	}
	m, ok := (*i.state).(map[string]interface{})
	if !ok {
		return nil
	}
	return m[key]
}

func (i *Interp) includePaths() []string {
	pathsAny, ok := i.lookupState("include_paths").([]interface{})
	if !ok {
		panic("include_paths not slice")
	}
	var paths []string
	for _, pathAny := range pathsAny {
		path, ok := pathAny.(string)
		if !ok {
			panic("path not string")
		}
		paths = append(paths, path)
	}
	return paths
}

func (i *Interp) slurps() map[string]interface{} {
	slurpsAny, _ := i.lookupState("slurps").(map[string]interface{})
	return slurpsAny
}

func (i *Interp) Options(v interface{}) Options {
	var opts Options
	_ = mapstructure.Decode(v, &opts)
	opts.ArrayTruncate = mathextra.MaxInt(0, opts.ArrayTruncate)
	opts.Depth = mathextra.MaxInt(0, opts.Depth)
	opts.AddrBase = mathextra.ClampInt(2, 36, opts.AddrBase)
	opts.SizeBase = mathextra.ClampInt(2, 36, opts.SizeBase)
	opts.LineBytes = mathextra.MaxInt(0, opts.LineBytes)
	opts.DisplayBytes = mathextra.MaxInt(0, opts.DisplayBytes)
	opts.Decorator = decoratorFromOptions(opts)
	opts.BitsFormatFn = bitsFormatFnFromOptions(opts)

	return opts
}

func (i *Interp) NewColorJSON(opts Options) (*colorjson.Encoder, error) {
	indent := 2
	if opts.Compact {
		indent = 0
	}

	return colorjson.NewEncoder(
		opts.Color,
		false,
		indent,
		func(v interface{}) interface{} {
			if v, ok := toValue(func() Options { return opts }, v); ok {
				return v
			}
			panic(fmt.Sprintf("toValue not a JQValue value: %#v", v))
		},
		colorjson.Colors{
			Reset:     []byte(ansi.Reset.SetString),
			Null:      []byte(opts.Decorator.Null.SetString),
			False:     []byte(opts.Decorator.False.SetString),
			True:      []byte(opts.Decorator.True.SetString),
			Number:    []byte(opts.Decorator.Number.SetString),
			String:    []byte(opts.Decorator.String.SetString),
			ObjectKey: []byte(opts.Decorator.ObjectKey.SetString),
			Array:     []byte(opts.Decorator.Array.SetString),
			Object:    []byte(opts.Decorator.Object.SetString),
		},
	), nil
}

var camelToSnakeRe = regexp.MustCompile(`[[:lower:]][[:upper:]]`)

// "AaaBbb" -> "aaa_bbb"
func camelToSnake(s string) string {
	return strings.ToLower(camelToSnakeRe.ReplaceAllStringFunc(s, func(s string) string {
		return s[0:1] + "_" + s[1:2]
	}))
}
func mapToStruct(m map[string]interface{}, v interface{}) error {
	ms, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		MatchName: func(mapKey, fieldName string) bool {
			return camelToSnake(fieldName) == mapKey
		},
		DecodeHook: func(
			f reflect.Type,
			t reflect.Type,
			data interface{}) (interface{}, error) {

			if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
				switch d := data.(type) {
				case string:
					return []byte(d), nil
				}
			} else {
				switch d := data.(type) {
				case *big.Int:
					return d.Uint64(), nil
				}
			}

			return data, nil
		},
		Result: v,
	})
	if err != nil {
		return err
	}

	if err := ms.Decode(m); err != nil {
		return err
	}

	return nil
}

func camelCaseMap(m map[string]interface{}) map[string]interface{} {
	nm := map[string]interface{}{}
	for k, v := range m {
		sk := camelToSnake(k)
		if vm, ok := v.(map[string]interface{}); ok {
			v = camelCaseMap(vm)
		} else {
			// TODO: error
			v, _ = gojqextra.ToGoJQValue(v)
		}
		nm[sk] = v
	}

	return nm
}

func structToMap(v interface{}) (map[string]interface{}, error) {
	m := map[string]interface{}{}
	ms, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result: &m,
	})
	if err != nil {
		return nil, err
	}
	if err := ms.Decode(v); err != nil {
		return nil, err
	}

	return camelCaseMap(m), nil
}
