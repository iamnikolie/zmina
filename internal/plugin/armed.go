package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/fsx"
)

// A press made through a plugin is one-shot. Each dialog event carries a nonce in its id (role:fingerprint:nonce), kept armed here
// until the first press made with it, which disarms it: a second press, a double tap, a retry of the plugin's own network call, a
// card that has been on a phone since yesterday, is refused with its own message and presses nothing, whatever the screen shows.
// The marks of a plugin are one small file (crew.Layout.PluginArmed), written by the delivery that told the event and by the choose
// that pressed, two processes, so each change is made under a short lock of its own, never the plugin's delivery lock.

// armedKeep is how long a mark is kept: a card older than this is no card.
const armedKeep = 2 * 24 * time.Hour

// Armed is a plugin's one-shot marks.
type Armed struct {
	Path, Lock string
}

type mark struct {
	Dialog  string    `json:"dialog"`
	At      time.Time `json:"at"`
	Pressed time.Time `json:"pressed,omitzero"`
}

// NewNonce makes the mark of one dialog event: 16 hex characters nobody can guess.
func NewNonce() string {
	var b [8]byte

	if _, err := rand.Read(b[:]); err != nil {
		panic("plugin: no randomness: " + err.Error())
	}

	return hex.EncodeToString(b[:])
}

// Arm records a nonce for the dialog an event is about, before the event goes: a press may come at once.
func (a Armed) Arm(nonce, dialog string, now time.Time) error {
	return a.update(now, func(m map[string]mark) error {
		m[nonce] = mark{Dialog: dialog, At: now}

		return nil
	})
}

// Consume disarms a nonce for a press at the dialog (role:fingerprint): the first press is allowed, and any other is refused with
// why — it was never told, it is of another question, it has been used.
func (a Armed) Consume(nonce, dialog string, now time.Time) error {
	return a.update(now, func(m map[string]mark) error {
		k, ok := m[nonce]

		switch {
		case !ok:
			return errors.New("this card is not armed: it was never told, or it is too old; answer in a terminal")
		case k.Dialog != dialog:
			return errors.New("this card is of another question: look again")
		case !k.Pressed.IsZero():
			return fmt.Errorf("this card was used already (pressed %s): a press goes once; answer in a terminal if the question is still there",
				k.Pressed.Local().Format("15:04:05"))
		}

		k.Pressed = now
		m[nonce] = k

		return nil
	})
}

// Rearm gives a press back that did not happen (the keys could not be sent).
func (a Armed) Rearm(nonce string, now time.Time) error {
	return a.update(now, func(m map[string]mark) error {
		if k, ok := m[nonce]; ok {
			k.Pressed = time.Time{}
			m[nonce] = k
		}

		return nil
	})
}

// Disarm forgets a nonce: its event was never taken.
func (a Armed) Disarm(nonce string, now time.Time) error {
	return a.update(now, func(m map[string]mark) error {
		delete(m, nonce)

		return nil
	})
}

// update changes the marks under the lock; a mark older than armedKeep goes with the change. An error from fn leaves the file as
// it was.
func (a Armed) update(now time.Time, fn func(map[string]mark) error) error {
	if err := fsx.MkdirAll(filepath.Dir(a.Path), 0o700); err != nil {
		return err
	}

	lock, err := os.OpenFile(a.Lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}

	defer func() { _ = lock.Close() }()

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}

	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	m := map[string]mark{}

	if f, err := os.Open(a.Path); err == nil {
		raw, _ := io.ReadAll(io.LimitReader(f, readMax))
		_ = f.Close()

		if json.Unmarshal(raw, &m) != nil || m == nil {
			m = map[string]mark{}
		}
	}

	if err := fn(m); err != nil {
		return err
	}

	for nonce, k := range m {
		if now.Sub(k.At) > armedKeep {
			delete(m, nonce)
		}
	}

	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}

	return fsx.WriteFile(a.Path, append(raw, '\n'), 0o600)
}
