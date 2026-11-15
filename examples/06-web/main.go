// Command web shows a run in a browser: where every step stands, the events
// as they are written, and buttons to approve a gate or stop the run. It is
// a viewer in a process of its own, and needs nothing from the tool that is
// running: only the path of its journal.
//
// Try it with 04-detached, in two terminals:
//
//	# terminal 1: start a run that waits at a gate
//	cd ../04-detached && go run . run
//
//	# terminal 2: watch it, then open http://127.0.0.1:8080
//	go run . -journal ../04-detached/upgrade.jsonl
//
// Any journal works: point -journal at 01-quickstart's site.jsonl, or
// 05-migrate's migrate.jsonl. The viewer can be started before the run, and
// stopping it never affects the run.
//
// Everything it does is one of four library calls: Inspect for where things
// stand, Follow for the events, and Approve and RequestStop for the buttons.
// It listens on this machine only, and must run as the user who owns the
// journal, as every viewer and approver must.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/ruffel/pipeline"
)

//go:embed index.html
var page []byte

const (
	// controlTimeout is how long a button waits for the run to act on it.
	controlTimeout = 30 * time.Second
	headerTimeout  = 5 * time.Second
)

func main() {
	journal := flag.String("journal", "../04-detached/upgrade.jsonl", "the journal of the run to show")
	addr := flag.String("addr", "127.0.0.1:8080", "where to listen; keep it on this machine")

	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	err := serve(ctx, *journal, *addr)

	cancel()

	if err != nil {
		fmt.Fprintf(os.Stderr, "web: %v\n", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, journal, addr string) error {
	v := viewer{journal: journal}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", v.index)
	mux.HandleFunc("GET /state", v.state)
	mux.HandleFunc("GET /events", v.events)
	mux.HandleFunc("POST /approve", v.approve)
	mux.HandleFunc("POST /stop", v.stop)

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: headerTimeout,
		// Requests end when the viewer does, so Ctrl-C closes open streams.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	go func() {
		<-ctx.Done()

		_ = srv.Close()
	}()

	fmt.Printf("showing %s at http://%s\n", journal, ln.Addr())

	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// viewer serves one journal.
type viewer struct{ journal string }

func (v viewer) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

// state answers with where everything stands: Inspect replays the journal,
// even while a run is writing it.
func (v viewer) state(w http.ResponseWriter, _ *http.Request) {
	st, err := pipeline.Inspect(v.journal)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	out := stateView{Journal: v.journal, Live: st.Live, LastSeq: st.LastSeq, Attempts: []attemptView{}}

	for _, a := range st.Attempts {
		out.Attempts = append(out.Attempts, attemptView{
			Attempt: a.Attempt, Rollback: a.Rollback, Outcome: string(a.Outcome), Started: a.Started,
		})
	}

	if st.Plan != nil {
		root := element(st, *st.Plan, false)
		out.Plan = &root
	}

	body, err := json.Marshal(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// events streams the journal's events as server-sent events, each one the
// JSON that the journal holds. The page asks for the events after the state
// it already has.
func (v viewer) events(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	stream := &eventStream{w: w, flush: http.NewResponseController(w).Flush}

	// Follow is tail -f for a journal: it delivers what is there, then what
	// a run writes, to any observer. The JSON observer writes each event as
	// the journal stores it. KeepFollowing waits for the next attempt when
	// one ends, so the page stays live across a resume or a rollback.
	err := pipeline.Follow(r.Context(), v.journal,
		pipeline.FollowOptions{After: after, KeepFollowing: true},
		pipeline.NewJSONObserver(stream))
	if err != nil && r.Context().Err() == nil {
		stream.send("failed", err.Error())
	}
}

// eventStream frames each line written to it as one server-sent event.
type eventStream struct {
	w     io.Writer
	flush func() error
}

func (s *eventStream) Write(line []byte) (int, error) {
	if _, err := fmt.Fprintf(s.w, "data: %s\n", line); err != nil {
		return 0, err
	}

	return len(line), s.flush()
}

func (s *eventStream) send(event, data string) {
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data)
	_ = s.flush()
}

// approve releases a gate, as Approve does from any process. It returns
// once the journal shows the run took the approval.
func (v viewer) approve(w http.ResponseWriter, r *http.Request) {
	v.control(w, r, func(ctx context.Context) error {
		return pipeline.Approve(ctx, v.journal, r.FormValue("gate"), "web")
	})
}

// stop asks the run to stop: gracefully, letting running steps finish, or
// with mode=cancel, cancelling them.
func (v viewer) stop(w http.ResponseWriter, r *http.Request) {
	mode := pipeline.StopGraceful
	if r.FormValue("mode") == string(pipeline.StopCancel) {
		mode = pipeline.StopCancel
	}

	v.control(w, r, func(ctx context.Context) error {
		return pipeline.RequestStop(ctx, v.journal, mode, "web")
	})
}

// control runs one request that changes the run. A page on another site
// could post here from the user's browser, so only the viewer's own page
// may.
func (v viewer) control(w http.ResponseWriter, r *http.Request, do func(context.Context) error) {
	if origin := r.Header.Get("Origin"); origin != "http://"+r.Host {
		http.Error(w, "requests must come from this page", http.StatusForbidden)

		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), controlTimeout)
	defer cancel()

	if err := do(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// stateView is what the page draws.
type stateView struct {
	Journal  string        `json:"journal"`
	Live     bool          `json:"live"`
	LastSeq  int64         `json:"lastSeq"`
	Attempts []attemptView `json:"attempts"`
	Plan     *elementView  `json:"plan,omitempty"`
}

type attemptView struct {
	Attempt  int       `json:"attempt"`
	Rollback bool      `json:"rollback"`
	Outcome  string    `json:"outcome"`
	Started  time.Time `json:"started"`
}

// elementView is one group, step or gate: what the plan says it is, and
// where the journal says it stands.
type elementView struct {
	Name        string        `json:"name"`
	Path        string        `json:"path"`
	Kind        string        `json:"kind"`
	Description string        `json:"description,omitempty"`
	Prompt      string        `json:"prompt,omitempty"`
	Handler     bool          `json:"handler,omitempty"`
	Status      string        `json:"status"`
	Reason      string        `json:"reason,omitempty"`
	Error       string        `json:"error,omitempty"`
	UndoError   string        `json:"undoError,omitempty"`
	Done        bool          `json:"done,omitempty"`
	Waiting     bool          `json:"waiting,omitempty"`
	Children    []elementView `json:"children,omitempty"`
}

// element joins a plan node to its state, and does the same for everything
// beneath it. A group's failure handler is drawn as its last child.
func element(st *pipeline.State, n pipeline.NodeSpec, handler bool) elementView {
	el := st.Element(n.Path)

	out := elementView{
		Name: n.Name, Path: n.Path, Kind: string(n.Kind), Description: n.Description, Prompt: n.Prompt,
		Handler: handler,
		Status:  string(el.Status), Reason: string(el.Reason), Done: el.Done,
		// A gate waits for nobody once its runner has gone.
		Waiting: el.Waiting && st.Live,
	}

	if el.Error != nil {
		out.Error = el.Error.Message
	}

	if el.UndoError != nil {
		out.UndoError = el.UndoError.Message
	}

	for _, c := range n.Children {
		out.Children = append(out.Children, element(st, c, false))
	}

	if n.OnFailure != nil {
		out.Children = append(out.Children, element(st, *n.OnFailure, true))
	}

	return out
}
