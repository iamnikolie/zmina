package mesh

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var sumA = strings.Repeat("ab", 32)

// A crew that tells no sum and holds nothing is told as it always was, byte for byte: the new fields are omitted when
// empty, so a node whose crews declare no nodes: changes nothing on the wire.
func TestCrewHealthWithoutSumOrHeldIsTheOldBytes(t *testing.T) {
	raw, err := json.Marshal([]CrewHealth{{Crew: "ops", TickAge: 10}, {Crew: "dev", Stopped: 5, TickAge: -1, Retired: true, MovedTo: "b/dev"}})
	require.NoError(t, err)
	assert.Equal(t, `[{"crew":"ops","tick_age":10},{"crew":"dev","tick_age":-1,"stopped":5,"retired":true,"moved_to":"b/dev"}]`, string(raw))

	raw, err = json.Marshal(CrewHealth{Crew: "ops", TickAge: 1, Sum: sumA, Held: []string{"dev"}})
	require.NoError(t, err)
	assert.Equal(t, `{"crew":"ops","tick_age":1,"sum":"`+sumA+`","held":["dev"]}`, string(raw))
}

// What a peer tells of the file's sum and the roles it holds is read only when it is plain: a sum is 64 hex digits, a role a
// plain lower-case word, each once; a reader from before the fields drops them with the rest it does not know.
func TestCrewHealthReadsSumAndHeldOnlyWhenPlain(t *testing.T) {
	got := readCrewHealth([]byte(`[{"crew":"ops","tick_age":1,"sum":"` + sumA + `","held":["dev","qa","dev","Bad Role","x;rm","a-b"]},` +
		`{"crew":"odd","tick_age":1,"sum":"not-a-sum","held":["` + strings.Repeat("x", 40) + `"]}]`))
	require.Len(t, got, 2)

	assert.Equal(t, sumA, got[0].Sum)
	assert.Equal(t, []string{"dev", "qa", "a-b"}, got[0].Held)
	assert.Empty(t, got[1].Sum)
	assert.Empty(t, got[1].Held)

	many := make([]string, 0, 100)
	for i := range 100 {
		many = append(many, "r"+strconv.Itoa(i))
	}

	raw, err := json.Marshal([]CrewHealth{{Crew: "ops", Held: many}})
	require.NoError(t, err)
	assert.Len(t, readCrewHealth(raw)[0].Held, heldMax)

	// A reader from before the fields: the struct it decodes into has no such keys, and the rest reads as it did.
	var old []struct {
		Crew    string `json:"crew"`
		TickAge int64  `json:"tick_age"`
	}

	raw, err = json.Marshal([]CrewHealth{{Crew: "ops", TickAge: 7, Sum: sumA, Held: []string{"dev"}}})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &old))
	assert.Equal(t, "ops", old[0].Crew)
	assert.EqualValues(t, 7, old[0].TickAge)
}

// The sum and the held roles travel in the signed hello, both ways, and are kept per peer.
func TestHelloTellsSumAndHeld(t *testing.T) {
	a := newOwner(t, "a")
	b := enroll(t, a, "b")

	addPeer(t, a, "b")

	a.Health = func() []CrewHealth { return []CrewHealth{{Crew: "ops", TickAge: 10, Sum: sumA}} }
	b.Health = func() []CrewHealth {
		return []CrewHealth{{Crew: "ops", TickAge: 5, Sum: strings.Repeat("cd", 32), Held: []string{"dev"}}}
	}

	net := &pipes{nodes: map[string]*Node{"b": b}, inboxes: map[string]*inbox{"b": {}}, down: map[string]bool{}}

	ss := a.Sync(context.Background(), net, []Discovery{Book{Node: a}}, (&inbox{}).deliver, "", time.Second, 0, time.Now)
	require.Len(t, ss, 1)
	require.NoError(t, ss[0].Err)

	seen, err := a.Peers()
	require.NoError(t, err)
	require.Len(t, seen[0].Crews, 1)
	assert.Equal(t, strings.Repeat("cd", 32), seen[0].Crews[0].Sum)
	assert.Equal(t, []string{"dev"}, seen[0].Crews[0].Held)

	other, err := b.Peers()
	require.NoError(t, err)
	require.Len(t, other[0].Crews, 1)
	assert.Equal(t, sumA, other[0].Crews[0].Sum)
	assert.Empty(t, other[0].Crews[0].Held)
}

// beforeCrewHealth, beforeCrewsMessage and beforeReadCrewHealth are what a node of the zmina from before sum and held did with the
// hello's crew health: a struct with its five fields, the message it verified the signature over, and the reader. They are
// copied here as they were, so that a test reads the new bytes the old way.
type beforeCrewHealth struct {
	Crew    string `json:"crew"`
	TickAge int64  `json:"tick_age"`
	Stopped int64  `json:"stopped,omitempty"`
	Retired bool   `json:"retired,omitempty"`
	MovedTo string `json:"moved_to,omitempty"`
}

func beforeCrewsMessage(side string, nonce, crews []byte) []byte {
	return []byte(fmt.Sprintf("zmina-crews-v1\x00%s\x00%x\x00%s", side, nonce, crews))
}

func beforeReadCrewHealth(data []byte) []beforeCrewHealth {
	if len(data) == 0 {
		return nil
	}

	var in []beforeCrewHealth
	if err := json.Unmarshal(data, &in); err != nil {
		return nil
	}

	out := make([]beforeCrewHealth, 0, len(in))

	for _, h := range in {
		if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`).MatchString(h.Crew) {
			continue
		}

		out = append(out, h)
	}

	return out
}

// A node of the zmina from before sum and held, calling a node that tells them, verifies the signature over the bytes as they were
// sent and reads the crews it knew: the new fields are dropped, and nothing about the session fails.
func TestACallerFromBeforeSumAndHeldReadsAndVerifiesTheNewHealth(t *testing.T) {
	owner := newOwner(t, "alpha")
	caller := enroll(t, owner, "beta")

	owner.Health = func() []CrewHealth {
		return []CrewHealth{{Crew: "ops", TickAge: 7, Stopped: 3, MovedTo: "box/ops", Sum: sumA, Held: []string{"dev", "qa"}},
			{Crew: "plain", TickAge: 2}}
	}

	client, server := net.Pipe()
	require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))

	go func() {
		defer func() { _ = server.Close() }()
		_ = owner.Serve(server, (&inbox{}).deliver, time.Now)
	}()

	// The hello of a caller from before: no capability words but the ones it had, and no crews of its own.
	c := newConn(client)
	challenge, err := nonce()
	require.NoError(t, err)
	require.NoError(t, c.send(wire{Op: "hello", V: protocolVersion, Cert: &caller.Cert, Nonce: challenge}))

	hi, err := c.expect("hello")
	require.NoError(t, err)
	require.NotEmpty(t, hi.Crews)
	assert.Contains(t, string(hi.Crews), `"sum":"`+sumA+`"`, "what was sent does carry the new fields")

	assert.True(t, ed25519.Verify(owner.Cert.Key, beforeCrewsMessage("server", challenge, hi.Crews), hi.CrewsSig),
		"the signature is over the bytes sent, so the old verification holds")

	got := beforeReadCrewHealth(hi.Crews)
	assert.Equal(t, []beforeCrewHealth{{Crew: "ops", TickAge: 7, Stopped: 3, MovedTo: "box/ops"}, {Crew: "plain", TickAge: 2}}, got)

	// And the session goes on: it proves its own key and is let in.
	require.NoError(t, c.send(wire{Op: "auth", Sig: ed25519.Sign(caller.Key, helloMessage("client", hi.Nonce))}))
	_, err = c.expect("ok")
	require.NoError(t, err)
}
