package errortrack

import (
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

// mainModule is the path of the module the binary was built from, read once:
// the functions under it are the app's own.
var mainModule = sync.OnceValue(func() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.Main.Path
	}
	return ""
})

// parseFrames reads a stack in runtime/debug.Stack's format: a "goroutine N
// [...]:" header, then a function line ("pkg.Func(args)") followed by a
// tab-indented "file:line +0x.." line for each call. Argument values are
// dropped, as are "created by" lines and, after a recovered panic, the frames
// newer than the panic site. Frames come oldest call first, as Sentry
// orders them; nil when nothing parses.
func parseFrames(stack []byte, mainModule string) []Frame {
	lines := strings.Split(string(stack), "\n")
	var frames []Frame
	for i := 0; i < len(lines); i++ {
		fn := strings.TrimSuffix(lines[i], "\r")
		if fn == "" || strings.HasPrefix(fn, "\t") || strings.HasPrefix(fn, "goroutine ") {
			continue
		}
		if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "\t") {
			continue // no location: "...additional frames elided..." or garbage
		}
		loc := lines[i+1]
		i++
		if strings.HasPrefix(fn, "created by ") {
			continue
		}
		name, ok := funcName(fn)
		if !ok {
			continue
		}
		file, line, ok := fileLine(loc)
		if !ok {
			continue
		}
		frames = append(frames, Frame{Function: name, File: file, Line: line, InApp: inApp(name, mainModule)})
	}
	// Newest first here: when the stack was taken after a recovered panic, drop
	// everything down to and including the runtime's panic frame (debug.Stack,
	// the recovering closure, panic itself), so the newest frame is the site.
	for i, f := range frames {
		if f.Function == "panic" || f.Function == "runtime.gopanic" {
			frames = frames[i+1:]
			break
		}
	}
	if len(frames) == 0 {
		return nil
	}
	for l, r := 0, len(frames)-1; l < r; l, r = l+1, r-1 {
		frames[l], frames[r] = frames[r], frames[l]
	}
	return frames
}

// funcName strips the argument list from "pkg.(*T).Method(0x1, {0x2})".
func funcName(s string) (string, bool) {
	if !strings.HasSuffix(s, ")") {
		return "", false
	}
	i := strings.LastIndex(s, "(")
	if i <= 0 {
		return "", false
	}
	return s[:i], true
}

// fileLine reads "\t/path/file.go:12 +0x3c"; the offset is optional.
func fileLine(s string) (string, int, bool) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " +0x"); i >= 0 {
		s = s[:i]
	}
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(s[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return s[:i], n, true
}

// inApp reports whether fn belongs to the main module: its package is the
// module's root ("mod.F") or below it ("mod/pkg.F"), or it is package main.
func inApp(fn, mainModule string) bool {
	if mainModule == "" {
		return false
	}
	return strings.HasPrefix(fn, mainModule+".") || strings.HasPrefix(fn, mainModule+"/") || strings.HasPrefix(fn, "main.")
}
