package cmd

import (
	"bytes"
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/ui"
)

const (
	// uiPoll is how often the page's stream looks at the shift; a snapshot goes out only when it changed.
	uiPoll = 2 * time.Second
	// uiPing keeps an idle stream open through proxies and tells the page it is still connected.
	uiPing = 15 * time.Second
	// uiCookie carries the token once a non-loopback page was opened with ?token=.
	uiCookie = "zmina_ui"
)

func (a *app) uiCommand() *cobra.Command {
	var (
		addr, token, lang string
		noOpen            bool
	)

	c := &cobra.Command{
		Use:   "ui",
		Short: "Serve a live status page of every crew under ~/.zmina on 127.0.0.1 and open it in the browser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" {
				token = os.Getenv("ZMINA_UI_TOKEN")
			}

			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return fmt.Errorf("--addr %s: %w", addr, err)
			}

			a.lang = uiLang(lang)
			loopback := isLoopback(host)
			if !loopback && token == "" {
				return fmt.Errorf("--addr %s is not a loopback address: the page writes notes to the dispatcher, so "+
					"it needs --token (or $ZMINA_UI_TOKEN) there", addr)
			}

			ln, err := net.Listen("tcp", addr)
			if err != nil {
				// One page for every crew: a second zmina ui opens the one that runs.
				if url := runningUI(addr, loopback, token); url != "" {
					fmt.Fprintln(a.out, "zmina ui already runs on", url)

					if !noOpen {
						openBrowser(url)
					}

					return nil
				}

				return err
			}

			srv := &http.Server{Handler: a.uiHandler(ln.Addr().String(), loopback, token),
				ReadHeaderTimeout: 10 * time.Second}

			url := "http://" + ln.Addr().String() + "/"
			if !loopback {
				url += "?token=" + token
			}

			fmt.Fprintln(a.out, "zmina ui on", url, "(Ctrl-C stops it)")

			if !noOpen {
				openBrowser(url)
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			go func() {
				<-ctx.Done()

				shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()

				_ = srv.Shutdown(shut)
			}()

			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				return err
			}

			return nil
		},
	}

	c.Flags().StringVar(&addr, "addr", "127.0.0.1:7517", "address to listen on; a non-loopback one needs --token")
	c.Flags().StringVar(&token, "token", "", "secret the page needs (?token= once, then a cookie); default "+
		"$ZMINA_UI_TOKEN")
	c.Flags().BoolVar(&noOpen, "no-open", false, "print the address, do not open a browser")
	c.Flags().StringVar(&lang, "lang", "", "the page's language, ru or en; default from $LC_ALL, $LC_MESSAGES or $LANG "+
		"(ru, uk and be read ru), else en; the page's ?lang= overrides it")

	return c
}

// uiLang is the page's language: the flag's, else the locale's — Russian for ru, uk and be, English otherwise.
func uiLang(flag string) string {
	loc := flag
	for _, env := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if loc == "" {
			loc = os.Getenv(env)
		}
	}

	switch strings.ToLower(loc[:min(2, len(loc))]) {
	case "ru", "uk", "be":
		return "ru"
	}

	return "en"
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// runningUI is the page's address when a zmina ui already answers on addr, else empty.
func runningUI(addr string, loopback bool, token string) string {
	url := "http://" + addr + "/"
	if !loopback {
		url += "?token=" + token
	}

	client := http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get("http://" + addr + "/api/ping?token=" + token)
	if err != nil {
		return ""
	}

	defer func() { _ = resp.Body.Close() }()

	var body [16]byte

	n, _ := resp.Body.Read(body[:])
	if resp.StatusCode != http.StatusOK || string(body[:n]) != "zmina" {
		return ""
	}

	return url
}

// uiCrew is one crew the page shows: Key names it in the page's calls (default, or its ~/.zmina/<key>/ config).
type uiCrew struct {
	Key, Path string
}

// uiCrews finds the crews: ~/.zmina/crew.yaml and every ~/.zmina/<name>/crew.yaml, looked up again on each call
// so a new crew shows without a restart; extra is a crew file given with --crew or $ZMINA_CREW.
func uiCrews(extra string) []uiCrew {
	var out []uiCrew

	seen := map[string]bool{}
	add := func(key, path string) {
		if _, err := os.Stat(path); err == nil && !seen[path] {
			seen[path] = true
			out = append(out, uiCrew{Key: key, Path: path})
		}
	}

	add("default", crew.ExpandHome("~/.zmina/crew.yaml"))

	files, _ := filepath.Glob(crew.ExpandHome("~/.zmina/*/crew.yaml"))
	sort.Strings(files)

	for _, f := range files {
		add(filepath.Base(filepath.Dir(f)), f)
	}

	if extra != "" {
		add(strings.TrimSuffix(filepath.Base(extra), filepath.Ext(extra)), extra)
	}

	return out
}

// forCrew is this run of the command line aimed at one crew file.
func (a *app) forCrew(path string) *app { return &app{out: a.out, err: io.Discard, crew: path} }

// crewApp resolves a page call's crew key to one of the crews found, never to a path the page names.
func (a *app) crewApp(key string) (*app, error) {
	for _, c := range uiCrews(a.extraCrew()) {
		if c.Key == key {
			return a.forCrew(c.Path), nil
		}
	}

	return nil, fmt.Errorf("no crew %q", key)
}

// extraCrew is a crew file named on the command line or in $ZMINA_CREW, besides the ones under ~/.zmina.
func (a *app) extraCrew() string {
	if a.crew != "" {
		return crew.ExpandHome(a.crew)
	}

	if a.config != "" {
		return crew.ConfigPath(a.config)
	}

	return os.Getenv("ZMINA_CREW")
}

func openBrowser(url string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}

	_ = exec.Command(name, url).Start()
}

// uiHandler serves the page, its stream and its few calls. bound is the listener's host:port.
func (a *app) uiHandler(bound string, loopback bool, token string) http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(ui.Files, "static")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "zmina") })
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, a.uiSnapshot()) })
	mux.HandleFunc("GET /api/stream", a.uiStream)
	mux.HandleFunc("GET /api/peek", func(w http.ResponseWriter, r *http.Request) {
		var text string

		c, err := a.crewApp(r.URL.Query().Get("crew"))
		if err == nil {
			err = c.readState(func(s *shift.Shift) error {
				var err error
				text, err = s.Peek(r.URL.Query().Get("role"), 30)

				return err
			})
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, text)
	})
	mux.HandleFunc("GET /api/task", func(w http.ResponseWriter, r *http.Request) {
		var out struct {
			Task task.Task      `json:"task"`
			Log  []events.Event `json:"log"`
		}

		c, err := a.crewApp(r.URL.Query().Get("crew"))
		if err == nil {
			err = c.readState(func(s *shift.Shift) error {
				t, err := taskStore(s.Crew).Get(r.URL.Query().Get("id"))
				if err != nil {
					return err
				}

				out.Task = t
				out.Log, _ = eventLog(s.Crew).Read(events.Filter{Task: t.ID, Last: 50})

				return nil
			})
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)

			return
		}

		writeJSON(w, out)
	})
	mux.HandleFunc("POST /api/todo", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Crew, ID, Action, Text string
		}

		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		if !map[string]bool{"yes": true, "done": true, "answer": true, "park": true, "ignore": true, "back": true,
			"drop": true}[in.Action] {
			http.Error(w, "action: yes, done, answer, park, ignore, back or drop", http.StatusBadRequest)

			return
		}

		var line string

		// The answer goes to the dispatcher of the item's own crew.
		c, err := a.crewApp(in.Crew)
		if err == nil {
			err = c.withState(func(s *shift.Shift) error {
				var err error
				line, err = answerTodo(s, "ui", in.Action, in.ID, in.Text)

				return err
			})
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)

			return
		}

		writeJSON(w, map[string]string{"done": line})
	})

	return guard(mux, bound, loopback, token)
}

// guard keeps the page to its owner. On loopback: the Host header must name this server (a DNS-rebinding page
// cannot read or write through the owner's browser). With a token: ?token= once sets a cookie, every request needs
// one of them. A write must carry X-Zmina, which a cross-site form or fetch cannot send without a preflight this
// server never answers.
func guard(next http.Handler, bound string, loopback bool, token string) http.Handler {
	_, port, _ := net.SplitHostPort(bound)
	hosts := map[string]bool{bound: true, "localhost:" + port: true, "127.0.0.1:" + port: true, "[::1]:" + port: true}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loopback && !hosts[r.Host] {
			http.Error(w, "unknown host", http.StatusForbidden)

			return
		}

		if token != "" {
			given := r.URL.Query().Get("token")
			if c, err := r.Cookie(uiCookie); given == "" && err == nil {
				given = c.Value
			}

			if subtle.ConstantTimeCompare([]byte(given), []byte(token)) != 1 {
				http.Error(w, "token needed", http.StatusUnauthorized)

				return
			}

			if r.URL.Query().Get("token") != "" {
				// Secure only over TLS: a LAN page over plain HTTP would never get the cookie back.
				http.SetCookie(w, &http.Cookie{Name: uiCookie, Value: token, Path: "/", HttpOnly: true,
					Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
			}
		}

		if r.Method != http.MethodGet && r.Header.Get("X-Zmina") != "1" {
			http.Error(w, "missing X-Zmina header", http.StatusForbidden)

			return
		}

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// uiStream sends the snapshot as server-sent events: at once, then whenever it changes.
func (a *app) uiStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	poll, ping := time.NewTicker(uiPoll), time.NewTicker(uiPing)
	defer poll.Stop()
	defer ping.Stop()

	var last []byte

	send := func() {
		raw, _ := json.Marshal(a.uiSnapshot())
		// The clock moves every poll; compare without it.
		if body := withoutNow(raw); !bytes.Equal(body, last) {
			last = body
			fmt.Fprintf(w, "event: state\ndata: %s\n\n", raw)
			flusher.Flush()
		}
	}

	send()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
			send()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func withoutNow(raw []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}

	delete(m, "now")
	out, _ := json.Marshal(m)

	return out
}

// uiRole is a role as the page shows it.
type uiRole struct {
	shift.View
	// Stuck is a busy role past stuck_minutes; Milestone the latest note on the first task it holds.
	Stuck     bool       `json:"stuck,omitempty"`
	Milestone *milestone `json:"milestone,omitempty"`
	Icon      string     `json:"icon"`
}

type milestone struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// uiCrewView is one crew's header and roles.
type uiCrewView struct {
	Key      string    `json:"key"`
	Name     string    `json:"name"`
	LastTick time.Time `json:"last_tick,omitzero"`
	Stopped  time.Time `json:"stopped,omitzero"`
	Stuck    int       `json:"stuck_minutes"`
	Roles    []uiRole  `json:"roles"`
	// Waiting is how many todo items the crew has; DoneToday how many tasks it closed as done today; Error why it
	// could not be read.
	Waiting   int    `json:"waiting"`
	DoneToday int    `json:"done_today"`
	Error     string `json:"error,omitempty"`
}

// Each list across crews carries the crew's key.
type (
	uiLater struct {
		Crew string `json:"crew"`
		later.Item
	}
	uiTask struct {
		Crew string `json:"crew"`
		task.Task
	}
	uiEvent struct {
		Crew string `json:"crew"`
		events.Event
		// Project and Title are the event's task's, when it has one.
		Project string `json:"project,omitempty"`
		Title   string `json:"title,omitempty"`
	}
)

// Where the page shows a piece of work: it waits on the owner (its todo item stands for it), it goes (in work, or
// waiting on a role of the crew), or it waits on someone outside the crew (or the owner set it aside).
const (
	laneYou    = "you"
	laneGo     = "go"
	laneOthers = "others"
)

// feedEvents are the events the page's feed tells; the rest (beats, keys, roles going up and down, a watch's notes
// to the dispatcher) stay in zmina log.
var feedEvents = map[string]bool{"task": true, "asked": true, "answered": true, "later": true, "blocked": true,
	"watch-failed": true, "start": true, "stop": true}

// feedMax is how many events the feed gets across crews.
const feedMax = 80

// uiWork is one active piece of work as the page leads with it: what, now, where, why, and what it needs from the
// owner.
type uiWork struct {
	Crew    string `json:"crew"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	Project string `json:"project,omitempty"`
	URL     string `json:"url,omitempty"`
	// Why is what was asked, in the owner's words.
	Why   string     `json:"why"`
	State task.State `json:"state"`
	Role  string     `json:"role"`
	// RoleState, Status and Since are the role's (up/down/…, busy/idle); Now and NowAt what it last said it does.
	RoleState shift.Presence `json:"role_state"`
	Status    string         `json:"status,omitempty"`
	Since     time.Time      `json:"since,omitzero"`
	Now       string         `json:"now,omitempty"`
	NowAt     time.Time      `json:"now_at,omitzero"`
	Prompt    bool           `json:"prompt,omitempty"`
	Milestone *milestone     `json:"milestone,omitempty"`
	// Needs is the todo item about this work, when it waits on the owner.
	Needs    *todoItem `json:"needs,omitempty"`
	Worktree string    `json:"worktree,omitempty"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	// Answered and Ignored are when the owner last answered the task or set it aside on the todo.
	Answered time.Time `json:"answered,omitzero"`
	Ignored  time.Time `json:"ignored,omitzero"`
	// Lane is where the page shows it: you, go or others; On who it waits on (a role, a person, or empty).
	Lane string `json:"lane"`
	On   string `json:"on,omitempty"`
}

type uiSnapshot struct {
	Version string       `json:"version"`
	Lang    string       `json:"lang"`
	Now     time.Time    `json:"now"`
	Work    []uiWork     `json:"work"`
	Crews   []uiCrewView `json:"crews"`
	Todo    []todoItem   `json:"todo"`
	Later   []uiLater    `json:"later"`
	Tasks   []uiTask     `json:"tasks"`
	Log     []uiEvent    `json:"log"`
	// Tints is every project the page names, with its tint (p1 … p5).
	Tints map[string]string `json:"tints"`
}

// tickerDead is how long a crew's ticker may be silent before the page shows it stopped.
const tickerDead = 2 * time.Minute

// uiSnapshot reads every crew as `zmina ls`, `todo ls`, `task ls` and `log` would, without the lock. The todo is
// one list across crews in the todo order; tasks, the parked list and the log are merged, newest first.
func (a *app) uiSnapshot() uiSnapshot {
	snap := uiSnapshot{Version: version, Lang: cmp.Or(a.lang, "en"), Now: time.Now(), Work: []uiWork{}, Crews: []uiCrewView{}, Todo: []todoItem{},
		Later: []uiLater{}, Tasks: []uiTask{}, Log: []uiEvent{}, Tints: map[string]string{}}
	pinned := map[string]string{}

	for _, uc := range uiCrews(a.extraCrew()) {
		cv := uiCrewView{Key: uc.Key, Name: uc.Key, Roles: []uiRole{}}

		err := a.forCrew(uc.Path).readState(func(s *shift.Shift) error {
			if s.Crew.Name != "" {
				cv.Name = s.Crew.Name
			}

			cv.LastTick, cv.Stopped, cv.Stuck = s.State.LastTick, s.State.Stopped, s.Crew.StuckMinutes

			// The first crew to pin a project's tint has it.
			for p, tint := range s.Crew.Tints() {
				if _, ok := pinned[p]; !ok {
					pinned[p] = tint
				}
			}

			a.readCrew(s, uc.Key, &cv, &snap)

			return nil
		})
		if err != nil {
			cv.Error = err.Error()
		}

		snap.Crews = append(snap.Crews, cv)
	}

	sortTodo(snap.Todo)
	// Work that needs the owner first, then what moves (busy), then the rest; newest change first.
	rank := func(w uiWork) int {
		switch {
		case w.Needs != nil:
			return 0
		case w.Status == "busy":
			return 1
		}

		return 2
	}
	sort.SliceStable(snap.Work, func(i, j int) bool {
		if ri, rj := rank(snap.Work[i]), rank(snap.Work[j]); ri != rj {
			return ri < rj
		}

		return snap.Work[i].Updated.After(snap.Work[j].Updated)
	})
	sort.SliceStable(snap.Tasks, func(i, j int) bool { return snap.Tasks[i].Created.After(snap.Tasks[j].Created) })
	sort.SliceStable(snap.Later, func(i, j int) bool { return snap.Later[i].Created.Before(snap.Later[j].Created) })
	sort.SliceStable(snap.Log, func(i, j int) bool { return snap.Log[i].TS.After(snap.Log[j].TS) })

	if len(snap.Log) > feedMax {
		snap.Log = snap.Log[:feedMax]
	}

	for _, p := range snap.projects() {
		snap.Tints[p] = ui.Tint(p, pinned)
	}

	// A crew whose ticker stopped first, then the one with the most waiting on the owner, then by name.
	dead := func(c uiCrewView) bool { return c.LastTick.IsZero() || snap.Now.Sub(c.LastTick) > tickerDead }
	sort.SliceStable(snap.Crews, func(i, j int) bool {
		ci, cj := snap.Crews[i], snap.Crews[j]
		if dead(ci) != dead(cj) {
			return dead(ci)
		}

		if ci.Waiting != cj.Waiting {
			return ci.Waiting > cj.Waiting
		}

		return ci.Name < cj.Name
	})

	return snap
}

// readCrew adds one crew's roles to cv and its todo, tasks, parked list and log to snap.
func (a *app) readCrew(s *shift.Shift, key string, cv *uiCrewView, snap *uiSnapshot) {
	now := s.Now()

	tasks, _, _ := taskStore(s.Crew).Scan()
	byID := map[string]task.Task{}
	byAny := map[string]task.Task{}

	for _, t := range tasks {
		byAny[t.ID] = t

		if t.State.Active() {
			snap.Tasks = append(snap.Tasks, uiTask{Crew: key, Task: t})
			byID[t.ID] = t
		}
	}

	views := s.Views()

	for _, v := range views {
		r := uiRole{View: v, Icon: roleIcon(v.Role)}
		r.Stuck = v.Status == "busy" && !v.Since.IsZero() && s.Crew.StuckMinutes > 0 &&
			now.Sub(v.Since) >= time.Duration(s.Crew.StuckMinutes)*time.Minute

		for _, id := range v.Tasks {
			if t, ok := byID[id]; ok && t.Note != "" {
				r.Milestone = &milestone{Text: t.Note, At: t.Updated}

				break
			}
		}

		cv.Roles = append(cv.Roles, r)
	}

	items, _ := todo(s.Crew, now, views)
	needs := map[string]todoItem{}

	for _, it := range items {
		it.Crew = key
		snap.Todo = append(snap.Todo, it)

		if it.Task != "" {
			needs[it.Task] = it
		}
	}

	byRole := map[string]shift.View{}
	for _, v := range views {
		byRole[v.Role] = v
	}

	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())

	for _, t := range tasks {
		if t.State == task.Done && !t.Updated.Before(today) {
			cv.DoneToday++
		}

		if !t.State.Active() {
			continue
		}

		v := byRole[t.Role]
		w := uiWork{Crew: key, ID: t.ID, Title: t.Headline(), Project: t.Where(), URL: t.URL, Why: t.Asked,
			State: t.State, Role: t.Role, RoleState: v.State, Status: v.Status, Since: v.Since, Now: v.Now,
			NowAt: v.NowAt, Prompt: v.Prompt, Worktree: t.Worktree, Created: t.Created, Updated: t.Updated,
			Answered: t.Answered, Ignored: t.Ignored}
		if t.Note != "" {
			w.Milestone = &milestone{Text: t.Note, At: t.Updated}
		}

		if it, ok := needs[t.ID]; ok {
			w.Needs = &it
		}

		w.Lane, w.On = lane(t, w.Needs != nil, s.Crew)
		snap.Work = append(snap.Work, w)
	}

	cv.Waiting = len(items)

	// The owner holds what a waiting task stands on, and an ask that holds up a role.
	for i := range snap.Todo {
		if it := &snap.Todo[i]; it.Crew == key {
			it.Holds = it.Blocks != "" || (it.Task != "" && byID[it.Task].State == task.Waiting && byID[it.Task].On == "")
		}
	}

	evs, _ := eventLog(s.Crew).Read(events.Filter{})

	for _, it := range laterStore(s.Crew).Parked() {
		snap.Later = append(snap.Later, uiLater{Crew: key, Item: it})
	}

	// The newest of what the feed tells; a watch's every run, keys and roles going up and down stay in zmina log.
	for i, n := len(evs)-1, 0; i >= 0 && n < feedMax; i-- {
		if e := evs[i]; feedEvents[e.Event] {
			ev := uiEvent{Crew: key, Event: e}
			if t, ok := byAny[e.Task]; ok {
				ev.Project, ev.Title = t.Where(), t.Headline()
			}

			snap.Log = append(snap.Log, ev)
			n++
		}
	}
}

// lane is where the page shows an active task, and who it waits on: a waiting task on the owner's todo is the
// todo item's; one the owner answered waits on the dispatcher; one set aside or waiting on a person outside the
// crew waits on others; open work, and work waiting on a role of the crew, goes.
func lane(t task.Task, onTodo bool, c *crew.Crew) (string, string) {
	if t.State != task.Waiting {
		return laneGo, ""
	}

	switch {
	case t.On != "":
		if _, ok := c.Roles[t.On]; ok {
			return laneGo, t.On
		}

		return laneOthers, t.On
	case onTodo:
		return laneYou, ""
	case t.Ignored.After(t.Updated):
		return laneOthers, ""
	}

	return laneGo, c.Dispatcher
}

// projects are the project names the page shows: the work's, the todo's and the feed's.
func (s *uiSnapshot) projects() []string {
	seen := map[string]bool{}

	var out []string

	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}

	for _, w := range s.Work {
		add(w.Project)
	}

	for _, it := range s.Todo {
		add(it.Project)
	}

	for _, e := range s.Log {
		add(e.Project)
	}

	return out
}

// roleIcon maps a role name to one of the page's line icons; crew files name roles freely, the rest are generic.
func roleIcon(role string) string {
	base, _, _ := shift.SplitInstance(role)

	switch {
	case base == "dispatcher", base == "designer", base == "director":
		return base
	case strings.HasPrefix(base, "dev"):
		return "developer"
	case strings.HasPrefix(base, "review"):
		return "reviewer"
	}

	return "generic"
}
