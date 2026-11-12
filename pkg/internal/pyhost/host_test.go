package pyhost

import (
	"context"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Runs the host suite in one child process per matrix interpreter, each starting that interpreter on its main thread
func TestInterpreterMatrix(t *testing.T) {
	needHost(t)
	dir, err := matrixDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range matrixVersions {
		t.Run(v, func(t *testing.T) {
			exe, err := matrixPython(dir, v)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run", "^TestHost", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), matrixEnv+"="+exe)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", exe, err, out)
			}
			if !strings.Contains(string(out), "--- PASS: TestHostIsolatedConfig") {
				t.Fatalf("%s ran no host case\n%s", exe, out)
			}
		})
	}
}

// Calls a Python callable named module.attr with Go arguments and converts the result
func callPy(t *testing.T, h *Host, module, attr string, args []any, kwargs map[string]any) (any, error) {
	t.Helper()
	var out any
	err := h.Do(func(g *GIL) error {
		m, err := g.Import(module)
		if err != nil {
			return err
		}
		defer g.Decref(m)
		r, err := g.CallMethod(m, attr, args, kwargs)
		if err != nil {
			return err
		}
		defer g.Decref(r)
		out, err = g.FromPython(r)
		return err
	})
	return out, err
}

// Reads module.attr and converts it
func attrPy(t *testing.T, h *Host, module, attr string) any {
	t.Helper()
	var out any
	err := h.Do(func(g *GIL) error {
		m, err := g.Import(module)
		if err != nil {
			return err
		}
		defer g.Decref(m)
		v, err := g.Attr(m, attr)
		if err != nil {
			return err
		}
		defer g.Decref(v)
		out, err = g.FromPython(v)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The embedded runtime starts isolated, venv aware and with the extra paths first on sys.path
func TestHostIsolatedConfig(t *testing.T) {
	h := needHost(t)
	in := h.Interpreter()
	sysmod := attrPy(t, h, "sys", "modules").(map[string]any)["sys"].(*Ref)
	for name, want := range map[string]any{"isolated": int64(1), "ignore_environment": int64(1), "no_user_site": int64(1), "utf8_mode": int64(1), "safe_path": true} {
		var got any
		err := h.Do(func(g *GIL) error {
			flags, err := g.Attr(sysmod.Ptr(), "flags")
			if err != nil {
				return err
			}
			defer g.Decref(flags)
			v, err := g.Attr(flags, name)
			if err != nil {
				return err
			}
			defer g.Decref(v)
			got, err = g.FromPython(v)
			return err
		})
		if err != nil || got != want {
			t.Fatalf("sys.flags.%s = %v (%v), want %v", name, got, err, want)
		}
	}
	if got := attrPy(t, h, "sys", "executable"); got != in.Executable {
		t.Fatalf("sys.executable = %v, want %s", got, in.Executable)
	}
	path := attrPy(t, h, "sys", "path").([]any)
	if !strings.HasSuffix(path[0].(string), "pyhost-extra") {
		t.Fatalf("sys.path[0] = %v", path[0])
	}
	vi := attrPy(t, h, "sys", "version_info").([]any)
	major, minor := vi[0], vi[1]
	if major != int64(in.Major) || minor != int64(in.Minor) {
		t.Fatalf("version_info %v.%v, interpreter %s", major, minor, in.Version())
	}
	if want := in.Major > 3 || in.Minor >= 14; hasInitConfig != want {
		t.Fatalf("PEP 741 path %v for %s", hasInitConfig, in.Version())
	}
	gil := in.Macros["Py_GIL_DISABLED"]
	if enabled, err := callPy(t, h, "sys", "_is_gil_enabled", nil, nil); err == nil && enabled == gil {
		t.Fatalf("sys._is_gil_enabled() = %v on a build with Py_GIL_DISABLED %v", enabled, gil)
	}
}

// Object header offsets come from live objects and the float payload reads back
func TestHostLayoutMeasured(t *testing.T) {
	needHost(t)
	if obTypeOffset == 0 || objectBasicSize < 16 || floatValOffset < objectBasicSize {
		t.Fatalf("layout ob_type@%d basic=%d float@%d", obTypeOffset, objectBasicSize, floatValOffset)
	}
}

// Go values cross into Python and back with their Python types and values intact
func TestHostConvertRoundTrip(t *testing.T) {
	h := needHost(t)
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	cases := []struct {
		in, want any
		typ      string
	}{
		{nil, nil, "NoneType"}, {true, true, "bool"}, {int64(-7), int64(-7), "int"}, {uint64(1 << 63), new(big.Int).SetUint64(1 << 63), "int"},
		{huge, huge, "int"}, {0.1, 0.1, "float"}, {"héllo", "héllo", "str"}, {[]byte{0, 1, 2}, []byte{0, 1, 2}, "bytes"},
		{[]any{int64(1), "a", nil}, []any{int64(1), "a", nil}, "list"}, {[]string{"x"}, []any{"x"}, "list"},
		{map[string]any{"k": []any{map[string]any{"n": 1.5}}}, map[string]any{"k": []any{map[string]any{"n": 1.5}}}, "dict"},
	}
	for _, c := range cases {
		err := h.Do(func(g *GIL) error {
			o, err := g.ToPython(c.in)
			if err != nil {
				return err
			}
			defer g.Decref(o)
			typ := pyObjectType(o)
			defer g.Decref(typ)
			if name := g.qualifiedName(typ); name != c.typ {
				t.Errorf("%#v became %s, want %s", c.in, name, c.typ)
			}
			back, err := g.FromPython(o)
			if err != nil {
				return err
			}
			if b, ok := back.(*big.Int); ok {
				if b.Cmp(c.want.(*big.Int)) != 0 {
					t.Errorf("%v came back %v", c.in, b)
				}
				return nil
			}
			if !reflect.DeepEqual(back, c.want) {
				t.Errorf("%#v came back %#v", c.in, back)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	tup, err := callPy(t, h, "builtins", "tuple", []any{[]any{int64(1), int64(2)}}, nil)
	if err != nil || !reflect.DeepEqual(tup, []any{int64(1), int64(2)}) {
		t.Fatalf("tuple came back %#v %v", tup, err)
	}
	ba, err := callPy(t, h, "builtins", "bytearray", []any{[]byte("ab")}, nil)
	if err != nil || !reflect.DeepEqual(ba, []byte("ab")) {
		t.Fatalf("bytearray came back %#v %v", ba, err)
	}
	obj, err := callPy(t, h, "builtins", "object", nil, nil)
	if ref, ok := obj.(*Ref); err != nil || !ok || ref.TypeName() != "object" {
		t.Fatalf("object() came back %#v %v", obj, err)
	}
}

// A Go function receives positional and keyword arguments from Python and its result converts back
func TestHostGoFunctionCalledFromPython(t *testing.T) {
	h := needHost(t)
	var gotArgs []any
	var gotKw map[string]any
	fn := NamedFunc{Name: "probe", Fn: func(g *GIL, args []any, kwargs map[string]any) (any, error) {
		gotArgs, gotKw = args, kwargs
		return map[string]any{"ok": true}, nil
	}}
	partial, err := callPy(t, h, "functools", "partial", []any{fn, int64(1)}, map[string]any{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := partial.(*Ref).Call([]any{"two"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, map[string]any{"ok": true}) || !reflect.DeepEqual(gotArgs, []any{int64(1), "two"}) || gotKw["k"] != "v" {
		t.Fatalf("out %v args %v kwargs %v", out, gotArgs, gotKw)
	}
	name, err := partial.(*Ref).GetAttr("func")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := name.(*Ref).GetAttr("__name__"); n != "probe" {
		t.Fatalf("__name__ %v", n)
	}
}

type probeObject struct{ calls int }

func (p *probeObject) TypeName() (string, string) { return "icebath.tests", "Probe" }

func (p *probeObject) GetAttr(g *GIL, name string) (any, bool, error) {
	switch name {
	case "value":
		return int64(42), true, nil
	case "bump":
		return NamedFunc{Name: "bump", Fn: func(g *GIL, args []any, kwargs map[string]any) (any, error) {
			p.calls++
			return int64(p.calls), nil
		}}, true, nil
	case "broken":
		return nil, false, &Exception{Type: "ValueError", Message: "broken attribute"}
	}
	return nil, false, nil
}

// Attribute lookup on a Go object reaches GetAttr, methods are cached and missing names raise AttributeError
func TestHostGoObjectAttributes(t *testing.T) {
	h := needHost(t)
	p := &probeObject{}
	err := h.Do(func(g *GIL) error {
		o, err := g.NewObject(p)
		if err != nil {
			return err
		}
		defer g.Decref(o)
		ref := g.NewRef(o)
		defer ref.Release()
		if v, err := ref.GetAttrLocked(g, "value"); err != nil || v != int64(42) {
			t.Errorf("value %v %v", v, err)
		}
		a, _ := g.Attr(o, "bump")
		b, _ := g.Attr(o, "bump")
		if a != b {
			t.Errorf("method not cached")
		}
		g.Decref(a)
		g.Decref(b)
		if _, err := g.Attr(o, "missing"); err == nil || err.Error() != "AttributeError: 'Probe' object has no attribute 'missing'" {
			t.Errorf("missing attribute error %v", err)
		}
		if _, err := g.Attr(o, "broken"); err == nil || err.Error() != "ValueError: broken attribute" {
			t.Errorf("broken attribute error %v", err)
		}
		if g.HasAttr(o, "missing") || !g.HasAttr(o, "value") {
			t.Errorf("hasattr")
		}
		typ := pyObjectType(o)
		defer g.Decref(typ)
		if name := g.qualifiedName(typ); name != "icebath.tests.Probe" {
			t.Errorf("type %s", name)
		}
		if _, err := g.Call(typ, nil, nil); err == nil || !strings.HasPrefix(err.Error(), "TypeError") {
			t.Errorf("Go object type is constructible from Python: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Dropping the last Python reference to a Go object removes its handle
func TestHostHandleReleasedOnDealloc(t *testing.T) {
	h := needHost(t)
	err := h.Do(func(g *GIL) error {
		handlesMu.RLock()
		before := len(handles)
		handlesMu.RUnlock()
		o, err := g.NewObject(&probeObject{})
		if err != nil {
			return err
		}
		handlesMu.RLock()
		during := len(handles)
		handlesMu.RUnlock()
		g.Decref(o)
		handlesMu.RLock()
		after := len(handles)
		handlesMu.RUnlock()
		if during != before+1 || after != before {
			t.Errorf("handles before %d during %d after %d", before, during, after)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A Python exception reaches Go with its qualified type, message and formatted traceback
func TestHostPythonExceptionCarriesTraceback(t *testing.T) {
	h := needHost(t)
	_, err := callPy(t, h, "json", "loads", []any{"{"}, nil)
	var pe *PyError
	if !errors.As(err, &pe) {
		t.Fatalf("error %T %v", err, err)
	}
	if pe.Type != "json.decoder.JSONDecodeError" || pe.Name() != "JSONDecodeError" || !strings.HasPrefix(pe.Message, "Expecting property name enclosed in double quotes") {
		t.Fatalf("type %q message %q", pe.Type, pe.Message)
	}
	if !strings.HasPrefix(pe.Traceback, "Traceback (most recent call last):\n") || !strings.Contains(pe.Traceback, "decoder.py") || !strings.Contains(pe.Traceback, "json.decoder.JSONDecodeError: Expecting property name") {
		t.Fatalf("traceback %q", pe.Traceback)
	}
}

// Go errors raise the named class, Python errors re-raise their own object and panics become RuntimeError
func TestHostGoErrorRaisedAsClass(t *testing.T) {
	h := needHost(t)
	var inner error
	cases := []struct {
		fn       Func
		typ, msg string
	}{
		{func(g *GIL, _ []any, _ map[string]any) (any, error) {
			return nil, &Exception{Type: "KeyError", Message: "k"}
		}, "KeyError", "'k'"},
		{func(g *GIL, _ []any, _ map[string]any) (any, error) {
			return nil, &Exception{Type: "asyncio.InvalidStateError", Message: "not ready"}
		}, "asyncio.exceptions.InvalidStateError", "not ready"},
		{func(g *GIL, _ []any, _ map[string]any) (any, error) {
			m, err := g.Import("builtins")
			if err != nil {
				return nil, err
			}
			defer g.Decref(m)
			_, inner = g.CallMethod(m, "int", []any{"x"}, nil)
			return nil, inner
		}, "ValueError", "invalid literal for int() with base 10: 'x'"},
		{func(g *GIL, _ []any, _ map[string]any) (any, error) {
			return nil, errors.New("plain go error")
		}, "RuntimeError", "plain go error"},
		{func(g *GIL, _ []any, _ map[string]any) (any, error) {
			panic("boom")
		}, "RuntimeError", "Go panic in host callback: boom"},
	}
	for _, c := range cases {
		_, err := callPy(t, h, "functools", "reduce", []any{NamedFunc{Name: "f", Fn: func(g *GIL, args []any, kw map[string]any) (any, error) { return c.fn(g, args, kw) }}, []any{int64(1), int64(2)}}, nil)
		var pe *PyError
		if !errors.As(err, &pe) || pe.Type != c.typ || !strings.HasPrefix(pe.Message, c.msg) {
			t.Fatalf("got %v, want %s: %s", err, c.typ, c.msg)
		}
	}
	var first *PyError
	if !errors.As(inner, &first) || !strings.Contains(first.Traceback, "ValueError") {
		t.Fatalf("inner %v", inner)
	}
}

// Many goroutines call one Python callable at once, each taking the GIL on its own locked thread
func TestHostRefCallFromGoroutines(t *testing.T) {
	h := needHost(t)
	fn := attrPy(t, h, "operator", "add")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				v, err := fn.(*Ref).Call([]any{int64(w), int64(i)}, nil)
				if err != nil || v != int64(w+i) {
					errs <- errors.New("bad sum")
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// A thread Python starts calls into Go while Go joins it from another goroutine
func TestHostPythonThreadCallsGo(t *testing.T) {
	h := needHost(t)
	ran := make(chan string, 1)
	target := NamedFunc{Name: "target", Fn: func(g *GIL, args []any, _ map[string]any) (any, error) {
		ran <- args[0].(string)
		return nil, nil
	}}
	th, err := callPy(t, h, "threading", "Thread", nil, map[string]any{"target": target, "args": []any{"from-thread"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := th.(*Ref).CallMethod("start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := th.(*Ref).CallMethod("join", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-ran; got != "from-thread" {
		t.Fatalf("thread passed %q", got)
	}
}

// A Go callback releases the GIL and a goroutine re-enters Python before the callback returns
func TestHostUnblockedReentry(t *testing.T) {
	h := needHost(t)
	fn := NamedFunc{Name: "reenter", Fn: func(g *GIL, _ []any, _ map[string]any) (any, error) {
		var v any
		var err error
		g.Unblocked(func() {
			done := make(chan struct{})
			go func() {
				defer close(done)
				v, err = callPy(t, h, "builtins", "len", []any{"four"}, nil)
			}()
			<-done
		})
		return v, err
	}}
	out, err := callPy(t, h, "functools", "reduce", []any{NamedFunc{Name: "r", Fn: func(g *GIL, args []any, kw map[string]any) (any, error) {
		return fn.Fn(g, args, kw)
	}}, []any{int64(0), int64(0)}}, nil)
	if err != nil || out != int64(4) {
		t.Fatalf("out %v %v", out, err)
	}
}

// Coroutines run on the loop thread, results, exceptions and cancellation come back to the waiting goroutine
func TestHostCallAsync(t *testing.T) {
	h := needHost(t)
	sleep := attrPy(t, h, "asyncio", "sleep").(*Ref)
	if v, err := sleep.CallAsync(context.Background(), []any{int64(0)}, map[string]any{"result": int64(5)}); err != nil || v != int64(5) {
		t.Fatalf("sleep result %v %v", v, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := sleep.CallAsync(ctx, []any{int64(30)}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled sleep %v", err)
	}
	coro, err := sleep.Call([]any{int64(5)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor := attrPy(t, h, "asyncio", "wait_for").(*Ref)
	_, err = waitFor.CallAsync(context.Background(), []any{coro}, map[string]any{"timeout": 0.01})
	var pe *PyError
	if !errors.As(err, &pe) || pe.Name() != "TimeoutError" {
		t.Fatalf("wait_for error %v", err)
	}
	pending, err := sleep.Call([]any{int64(0), "awaited"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := pending.(*Ref).Await(context.Background()); err != nil || v != "awaited" {
		t.Fatalf("await %v %v", v, err)
	}
	add := attrPy(t, h, "operator", "add").(*Ref)
	if v, err := add.CallAsync(context.Background(), []any{int64(2), int64(3)}, nil); err != nil || v != int64(5) {
		t.Fatalf("plain callable %v %v", v, err)
	}
}
