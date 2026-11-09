package cmd

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"rsc.io/qr"

	"github.com/iamnikolie/zmina/internal/mesh"
)

// zmina connect signs a phone or another browser in to this machine's cockpit with no notifier: it asks the running
// zmina ui for a one-time six-digit code, showing the handoff secret ui.json (0600) holds, and prints the code with
// the addresses the cockpit answers on. The code is made and kept by the cockpit, in its memory only: it is in no
// file, log, argv or environment, and leaves the cockpit only in the answer to this call.

// cockpitAddr is one address a cockpit answers on, and what reaches it there.
type cockpitAddr struct {
	URL  string `json:"url"`
	Kind string `json:"kind"`
}

const (
	addrTailnet = "tailnet"
	addrNetwork = "network"
	addrProxy   = "proxy"
	addrLocal   = "this machine"
)

// interfaceAddrs is this machine's interface addresses (a variable for tests).
var interfaceAddrs = net.InterfaceAddrs

// tailnet is the address range Tailscale hands out (CGNAT space).
var tailnet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// cockpitAddrs is where a cockpit bound to bound answers, phone-reachable first: tailnet, then the local network,
// then the private reverse proxies of --allow-host, and this machine's own address last. A cockpit bound to every
// interface (0.0.0.0, ::) answers on each IPv4 address of this machine.
func cockpitAddrs(bound string, allowHosts []string) []cockpitAddr {
	host, port, err := net.SplitHostPort(bound)
	if err != nil {
		return nil
	}

	out := []cockpitAddr{}
	seen := map[string]bool{}
	add := func(ip net.IP) {
		kind := addrNetwork

		switch {
		case ip.IsLoopback():
			kind = addrLocal
		case ip.IsLinkLocalUnicast(), ip.IsUnspecified():
			return
		case tailnet.Contains(ip):
			kind = addrTailnet
		}

		url := "http://" + net.JoinHostPort(ip.String(), port) + "/"
		if !seen[url] {
			seen[url] = true
			out = append(out, cockpitAddr{URL: url, Kind: kind})
		}
	}

	switch ip := net.ParseIP(host); {
	case ip != nil && ip.IsUnspecified():
		addrs, _ := interfaceAddrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				add(n.IP.To4())
			}
		}

		add(net.IPv4(127, 0, 0, 1))
	case ip != nil:
		add(ip)
	default:
		out = append(out, cockpitAddr{URL: "http://" + net.JoinHostPort(host, port) + "/", Kind: addrLocal})
	}

	for _, h := range allowHosts {
		out = append(out, cockpitAddr{URL: "https://" + h + "/", Kind: addrProxy})
	}

	rank := map[string]int{addrTailnet: 0, addrNetwork: 1, addrProxy: 2, addrLocal: 3}
	slices.SortStableFunc(out, func(a, b cockpitAddr) int { return rank[a.Kind] - rank[b.Kind] })

	return out
}

// forwardHeaders are the headers a proxy adds: a request with any of them came through one.
var forwardHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"}

// fromThisMachine reports whether a request came from this machine: over loopback, or from the very address it
// came in on (a cockpit bound to a tailnet address, asked by a zmina connect beside it). A request through a proxy
// is not, though it arrives over loopback: one naming a --allow-host host, or carrying a forwarding header — a
// local reverse proxy makes every remote request look local.
func fromThisMachine(r *http.Request, proxyHosts []string) bool {
	if slices.Contains(proxyHosts, r.Host) {
		return false
	}

	for _, h := range forwardHeaders {
		if r.Header.Get(h) != "" {
			return false
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	if ip.IsLoopback() {
		return true
	}

	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}

	lhost, _, err := net.SplitHostPort(local.String())

	return err == nil && net.ParseIP(lhost).Equal(ip)
}

// serveConnect answers POST /api/signin/connect, the call of zmina connect: for the holder of the handoff secret on
// this machine, a fresh sign-in code and the cockpit's addresses. It reports whether it answered the request.
func serveConnect(w http.ResponseWriter, r *http.Request, l *uiLogins, s *uiSignin, bound string, allowHosts []string) bool {
	if l == nil || r.URL.Path != "/api/signin/connect" {
		return false
	}

	w.Header().Set("Cache-Control", "no-store")

	secret := r.Header.Get("X-Zmina-Handoff")
	if r.Method != http.MethodPost || r.Header.Get("X-Zmina") != "1" || l.handoff == "" || !fromThisMachine(r, allowHosts) ||
		subtle.ConstantTimeCompare([]byte(secret), []byte(l.handoff)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)

		return true
	}

	addrs := cockpitAddrs(bound, allowHosts)

	if s == nil {
		// No token: the cockpit is open on this machine only, and there is nothing to sign in to.
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": "no token", "addrs": addrs})

		return true
	}

	code, wait, err := s.connect()

	switch {
	case errors.Is(err, errSigninWait):
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(w, map[string]any{"wait": int(wait.Round(time.Second) / time.Second)})
	case err != nil:
		http.Error(w, "no code", http.StatusInternalServerError)
	default:
		writeJSON(w, map[string]any{"code": code, "ttl": int(signinTTL / time.Second), "tries": signinTries,
			"addrs": addrs})
	}

	return true
}

// connectAnswer is what the cockpit answers zmina connect.
type connectAnswer struct {
	Code  string        `json:"code"`
	TTL   int           `json:"ttl"`
	Tries int           `json:"tries"`
	Wait  int           `json:"wait"`
	Addrs []cockpitAddr `json:"addrs"`
}

func (a *app) connectCommand() *cobra.Command {
	var showQR bool

	c := &cobra.Command{
		Use:   "connect [node]",
		Short: "Sign a phone or another browser in to this machine's cockpit: its addresses and a one-time code",
		Long: "Asks the zmina ui running on this machine for a one-time six-digit code (single use, 5 minutes, 5 wrong " +
			"tries) and prints it with the addresses the cockpit answers on; type the code on the cockpit's sign-in " +
			"page. Needs no notifier. The cockpit is per machine: no crew is named. With a node, runs zmina connect " +
			"there over this node's address for it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				ran, err := a.connectFar(cmd, args[0], showQR)
				if ran || err != nil {
					return err
				}
			}

			return a.connectHere(showQR)
		},
	}

	c.Flags().BoolVar(&showQR, "qr", false, "also draw the first address as a QR code (the address only, never the code)")

	return c
}

// connectFar runs zmina connect on node; false when node is this one.
func (a *app) connectFar(cmd *cobra.Command, node string, showQR bool) (bool, error) {
	me, err := openNode()
	if err != nil {
		return false, fmt.Errorf("%s: this machine is no node (zmina node init): %w", node, err)
	}

	if node == me.Name {
		return false, nil
	}

	addr, err := peerAddr(me, node)
	if err != nil {
		return false, err
	}

	words := []string{"connect"}
	if showQR {
		words = append(words, "--qr")
	}

	c, err := mesh.OwnerCommand(context.Background(), addr, false, words...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", node, err)
	}

	c.Stdout, c.Stderr = a.out, a.err

	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 {
			// The far zmina said why on stderr already.
			cmd.SilenceErrors, cmd.SilenceUsage = true, true

			return true, &exitError{code: ee.ExitCode(), msg: fmt.Sprintf("%s: exit %d", node, ee.ExitCode())}
		}

		return true, fmt.Errorf("%s: %w", node, err)
	}

	return true, nil
}

// localURL reports whether an address names this machine: loopback, or one of its interface addresses. A recorded
// cockpit elsewhere (a LAN address this machine no longer holds) is not asked: the handoff secret would go to
// whatever answers there now.
func localURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}

	host := u.Hostname()
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	if ip.IsLoopback() {
		return true
	}

	addrs, _ := interfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}

	return false
}

// alive reports whether a process runs under pid.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}

	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}

// startUIHint is how to start a cockpit a phone reaches.
func startUIHint() string {
	host := "<tailnet ip>"

	for _, ad := range cockpitAddrs("0.0.0.0:7517", nil) {
		if ad.Kind == addrTailnet {
			host = strings.TrimSuffix(strings.TrimPrefix(ad.URL, "http://"), ":7517/")

			break
		}
	}

	return "start it where a phone reaches it, with a token (in its environment, not its arguments):\n" +
		"  ZMINA_UI_TOKEN=<long secret> zmina ui --addr " + host + ":7517 --no-open\n" +
		"then run zmina connect again"
}

func (a *app) connectHere(showQR bool) error {
	r, ok := recordedCockpit()
	if !ok {
		return errors.New("no zmina ui runs on this machine; " + startUIHint())
	}

	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(r.URL, "/")+"/api/signin/connect", nil)
	if err != nil {
		return err
	}

	req.Header.Set("X-Zmina", "1")
	req.Header.Set("X-Zmina-Handoff", r.Handoff)

	resp, err := uiProbe().Do(req)
	if err != nil {
		return fmt.Errorf("zmina ui recorded at %s does not answer; %s", r.URL, startUIHint())
	}

	defer func() { _ = resp.Body.Close() }()

	var got connectAnswer

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	parsed := json.Unmarshal(body, &got) == nil

	switch {
	case resp.StatusCode == http.StatusConflict && parsed:
		return fmt.Errorf("zmina ui on %s has no token: it opens on this machine only and has nothing to sign in "+
			"to; %s", r.URL, startUIHint())
	case resp.StatusCode == http.StatusTooManyRequests && parsed:
		return fmt.Errorf("a code was made less than a minute ago, or ten this hour: wait %ds, or use the code "+
			"printed last", got.Wait)
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("zmina ui on %s refused the handoff secret of %s: it is not the cockpit that wrote it; "+
			"restart zmina ui", r.URL, uiStatePath())
	case resp.StatusCode != http.StatusOK || !parsed || len(got.Code) != 6:
		return fmt.Errorf("zmina ui on %s makes no connect codes (an older zmina ui?): restart it with this zmina", r.URL)
	}

	a.printConnect(got, showQR)

	return nil
}

func (a *app) printConnect(got connectAnswer, showQR bool) {
	reachable := []cockpitAddr{}

	fmt.Fprintln(a.out, "Open on the phone:")

	for _, ad := range got.Addrs {
		fmt.Fprintf(a.out, "  %-40s %s\n", ad.URL, ad.Kind)

		if ad.Kind != addrLocal {
			reachable = append(reachable, ad)
		}
	}

	if len(reachable) == 0 {
		fmt.Fprintln(a.err, "zmina: the cockpit answers on this machine only: a phone cannot reach it; "+startUIHint())
	}

	if showQR && len(reachable) > 0 {
		if q, err := qrBlock(reachable[0].URL); err == nil {
			fmt.Fprint(a.out, q)
		}
	}

	// The code goes last, on a line of its own, under what to do with it.
	fmt.Fprintf(a.out, "\nType the code on the sign-in page. It works once, for %d minutes; %d wrong tries end it.\n",
		got.TTL/60, got.Tries)
	fmt.Fprintf(a.out, "Code: %s\n", got.Code)
}

// qrBlock draws text as a QR code for a terminal: two rows of modules per line in upper half blocks, black and
// white set explicitly so it reads on a dark and a light terminal alike, with the quiet zone around it.
func qrBlock(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}

	const quiet = 2

	black := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
			return false
		}

		return code.Black(x, y)
	}

	color := func(b bool) int {
		if b {
			return 0
		}

		return 7
	}

	var sb strings.Builder

	n := code.Size + 2*quiet
	for y := 0; y < n; y += 2 {
		sb.WriteString("  ")

		for x := range n {
			fmt.Fprintf(&sb, "\x1b[3%d;4%dm▀", color(black(x, y)), color(black(x, y+1)))
		}

		sb.WriteString("\x1b[0m\n")
	}

	return sb.String(), nil
}
