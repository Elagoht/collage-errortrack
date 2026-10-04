package errortrack

import (
	"runtime/debug"
	"strings"
	"testing"
)

const testModule = "github.com/Elagoht/collage-errortrack"

// stackHere returns the stack of its caller's goroutine.
func stackHere() []byte { return debug.Stack() }

func TestFrames(t *testing.T) {
	frames := parseFrames(stackHere(), testModule)
	if len(frames) < 3 {
		t.Fatalf("got %d frames, want at least 3: %+v", len(frames), frames)
	}
	test, helper := -1, -1
	for i, f := range frames {
		if f.Function == "" || f.File == "" || f.Line <= 0 {
			t.Errorf("frame %d incomplete: %+v", i, f)
		}
		if strings.HasSuffix(f.Function, ")") {
			t.Errorf("frame %d keeps its argument list: %q", i, f.Function)
		}
		switch {
		case f.Function == testModule+".TestFrames":
			test = i
			if !f.InApp {
				t.Errorf("the test's own frame is not in the app: %+v", f)
			}
		case f.Function == testModule+".stackHere":
			helper = i
		case strings.HasPrefix(f.Function, "runtime"), strings.HasPrefix(f.Function, "testing."):
			if f.InApp {
				t.Errorf("a standard library frame is in the app: %+v", f)
			}
		}
	}
	if test < 0 || helper < 0 {
		t.Fatalf("missing frames (test %d, helper %d): %+v", test, helper, frames)
	}
	if test > helper {
		t.Errorf("frames are not oldest first: test at %d, its callee at %d", test, helper)
	}
	if last := frames[len(frames)-1]; last.Function != "runtime/debug.Stack" {
		t.Errorf("the newest frame is %q, want runtime/debug.Stack", last.Function)
	}
	for _, f := range frames {
		if strings.HasPrefix(f.Function, "created by") {
			t.Errorf("a created-by line became a frame: %+v", f)
		}
	}
}

func TestFrames_NoModule(t *testing.T) {
	for _, f := range parseFrames(stackHere(), "") {
		if f.InApp {
			t.Errorf("with no main module, %q is in the app", f.Function)
		}
	}
}

func TestFrames_Method(t *testing.T) {
	stack := []byte("goroutine 7 [running]:\n" +
		"example.com/m/pkg.(*T).Do(0x1400012, {0x2, 0x3})\n" +
		"\t/src/pkg/t.go:12 +0x3c\n" +
		"example.com/m/pkg.inlined(...)\n" +
		"\t/src/pkg/t.go:30\n" +
		"...additional frames elided...\n" +
		"created by example.com/m/pkg.Start in goroutine 1\n" +
		"\t/src/pkg/start.go:5 +0x10\n")
	got := parseFrames(stack, "example.com/m")
	want := []Frame{
		{Function: "example.com/m/pkg.inlined", File: "/src/pkg/t.go", Line: 30, InApp: true},
		{Function: "example.com/m/pkg.(*T).Do", File: "/src/pkg/t.go", Line: 12, InApp: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFrames_PrefixIsNotTheModule(t *testing.T) {
	stack := []byte("goroutine 1 [running]:\n" +
		"github.com/Elagoht/collage.Run()\n" +
		"\t/src/c.go:1 +0x1\n")
	got := parseFrames(stack, "github.com/Elagoht/collage-errortrack")
	if len(got) != 1 || got[0].InApp {
		t.Errorf("a module sharing a prefix counts as the app: %+v", got)
	}
}

func TestFrames_Garbage(t *testing.T) {
	for _, s := range []string{"nonsense\n\t", "", "goroutine 1 [running]:\n", "f()\n\tnofile\n", "f()\n\tx.go:-3 +0x1\n", "\t\t\n(\n"} {
		if got := parseFrames([]byte(s), ""); got != nil {
			t.Errorf("parseFrames(%q) = %+v, want nil", s, got)
		}
	}
	if got := parseFrames(nil, testModule); got != nil {
		t.Errorf("parseFrames(nil) = %+v, want nil", got)
	}
}

// panicHere panics and returns the stack recovered from it.
func panicHere() (stack []byte) {
	defer func() {
		_ = recover()
		stack = debug.Stack()
	}()
	panic("here")
}

func TestFrames_FromThePanicSite(t *testing.T) {
	frames := parseFrames(panicHere(), testModule)
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	if last := frames[len(frames)-1]; last.Function != testModule+".panicHere" {
		t.Errorf("the newest frame is %q, want the function that panicked", last.Function)
	}
	for _, f := range frames {
		if f.Function == "panic" || f.Function == "runtime/debug.Stack" || strings.HasPrefix(f.Function, testModule+".panicHere.") {
			t.Errorf("a frame newer than the panic site survived: %q", f.Function)
		}
	}
}
