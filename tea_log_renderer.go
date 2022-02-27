package tea_log_renderer

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/ansi/compressor"
	"github.com/muesli/reflow/truncate"
	"github.com/muesli/reflow/wordwrap"

	te "github.com/muesli/termenv"
)

func clearLine(w io.Writer) {
	fmt.Fprintf(w, te.CSI+te.EraseLineSeq, 2)
}

func cursorUp(w io.Writer, numUp int) {
	fmt.Fprintf(w, te.CSI+te.CursorUpSeq, numUp)
}

func cursorDown(w io.Writer, numLines int) {
	fmt.Fprintf(w, te.CSI+te.CursorDownSeq, 1)
}

const (
	// defaultFramerate specifies the maximum interval at which we should
	// update the view.
	defaultFramerate = time.Second / 60
)

// Fork of standardRenderer from tea but able to write new lines as a log
// See  https://github.com/charmbracelet/bubbletea/blob/42cd4c31919c2bfc8c06761634dedffcae9ca369/tea.go
type TeaLogRenderer struct {
	out                io.Writer
	queuedMessageLines []string
	buf                bytes.Buffer
	framerate          time.Duration
	ticker             *time.Ticker
	mtx                *sync.Mutex
	done               chan struct{}
	lastRender         string
	linesRendered      int
	useANSICompressor  bool
	once               sync.Once

	// renderer dimensions; usually the size of the window
	width  int
	height int

	// lines explicitly set not to render
	ignoreLines map[int]struct{}
}

// newRenderer creates a new renderer. Normally you'll want to initialize it
// with os.Stdout as the first argument.
func NewTeaLogRenderer(out io.Writer, mtx *sync.Mutex, useANSICompressor bool) TeaLogRenderer {
	r := TeaLogRenderer{
		out:                out,
		mtx:                mtx,
		framerate:          defaultFramerate,
		useANSICompressor:  useANSICompressor,
		queuedMessageLines: []string{},
	}
	if r.useANSICompressor {
		r.out = &compressor.Writer{Forward: out}
	}
	return r
}

// start starts the renderer.
func (r *TeaLogRenderer) Start() {
	if r.ticker == nil {
		r.ticker = time.NewTicker(r.framerate)
	}
	r.done = make(chan struct{})
	go r.listen()
}

// stop permanently halts the renderer, rendering the final frame.
func (r *TeaLogRenderer) Stop() {
	r.flush()
	clearLine(r.out)
	r.once.Do(func() {
		close(r.done)
	})

	if r.useANSICompressor {
		if w, ok := r.out.(io.WriteCloser); ok {
			_ = w.Close()
		}
	}
}

// kill halts the renderer. The final frame will not be rendered.
func (r *TeaLogRenderer) Kill() {
	clearLine(r.out)
	r.once.Do(func() {
		close(r.done)
	})
}

// listen waits for ticks on the ticker, or a signal to stop the renderer.
func (r *TeaLogRenderer) listen() {
	for {
		select {
		case <-r.ticker.C:
			if r.ticker != nil {
				r.flush()
			}
		case <-r.done:
			r.ticker.Stop()
			r.ticker = nil
			return
		}
	}
}

// flush renders the buffer.
func (r *TeaLogRenderer) flush() {
	r.mtx.Lock()
	defer r.mtx.Unlock()

	if r.buf.Len() == 0 || r.buf.String() == r.lastRender {
		// Nothing to do
		return
	}

	// Output buffer
	out := new(bytes.Buffer)

	newLines := strings.Split(r.buf.String(), "\n")
	realLinesRendered := len(newLines)
	oldLines := strings.Split(r.lastRender, "\n")
	skipLines := make(map[int]struct{})

	if len(r.queuedMessageLines) > 0 {
		newLines = append(r.queuedMessageLines, newLines...)
		r.queuedMessageLines = []string{}
	}

	// Clear any lines we painted in the last render.
	if realLinesRendered > 0 {
		skipped := 0
		for i := r.linesRendered - 1; i > 0; i-- {
			// If the number of lines we want to render hasn't increased and
			// new line is the same as the old line we can skip rendering for
			// this line as a performance optimization.
			if (len(newLines) <= len(oldLines)) && (len(newLines) > i && len(oldLines) > i) && (newLines[i] == oldLines[i]) {
				// skipLines[i] = struct{}{}
				skipped += 1
			} else {
				cursorUp(out, skipped+1)
				skipped = 0
				// otherwise, clear the line so the new rendering can write into it
				clearLine(out)
			}
		}
		if skipped >= 1 {
			cursorUp(out, skipped)
		}

		// here, the cursor is back at the start of the old buffer, with some lines cleared
	}

	// Paint new lines
	for i := 0; i < len(newLines); i++ {
		if _, skip := skipLines[i]; skip {
			// Unless this is the last line, move the cursor down.
			if i < len(newLines)-1 {
				cursorDown(out, 1)
			}
		} else {
			line := newLines[i]

			// Truncate lines wider than the width of the window to avoid
			// wrapping, which will mess up rendering. If we don't have the
			// width of the window this will be ignored.
			//
			// Note that on Windows we only get the width of the window on
			// program initialization, so after a resize this won't perform
			// correctly (signal SIGWINCH is not supported on Windows).
			if r.width > 0 {
				line = truncate.String(line, uint(r.width))
			}

			_, _ = io.WriteString(out, line)

			if i < len(newLines)-1 {
				_, _ = io.WriteString(out, "\r\n")
			}
		}
	}
	r.linesRendered = realLinesRendered

	_, _ = r.out.Write(out.Bytes())
	r.lastRender = r.buf.String()
	r.buf.Reset()
}

// write writes to the internal buffer. The buffer will be outputted via the
// ticker which calls flush().
func (r *TeaLogRenderer) Write(s string) {
	r.mtx.Lock()
	defer r.mtx.Unlock()
	r.buf.Reset()

	// If an empty string was passed we should clear existing output and
	// rendering nothing. Rather than introduce additional state to manage
	// this, we render a single space as a simple (albeit less correct)
	// solution.
	if s == "" {
		s = " "
	}

	_, _ = r.buf.WriteString(s)
}

func (r *TeaLogRenderer) Repaint() {
	r.lastRender = ""
}

// handleMessages handles internal messages for the renderer.
func (r *TeaLogRenderer) HandleMessages(msg tea.Msg) {
	// fmt.Printf("\n%#v\n", msg)
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		r.mtx.Lock()
		r.width = msg.Width
		r.height = msg.Height
		r.mtx.Unlock()

	case appendLogMsg:

		// prep the message text before locking
		messageText := msg.messageText
		if r.width != 0 {
			messageText = wordwrap.String(messageText, r.width)
		}
		messageLines := strings.Split(messageText, "\n")

		r.mtx.Lock()
		r.queuedMessageLines = append(r.queuedMessageLines, messageLines...)
		r.mtx.Unlock()
	}
}

// AltScreen is always false for a log renderer, because we never
// want to go fullscreen
func (n *TeaLogRenderer) AltScreen() bool { return false }

// Never setaltscreen
func (n *TeaLogRenderer) SetAltScreen(b bool) {}

type appendLogMsg struct {
	messageText string
}

// AppendLog adds a line to the log, shifting the rendering area down.
func AppendLog(messageText string) tea.Msg {
	return appendLogMsg{
		messageText: messageText,
	}
}
