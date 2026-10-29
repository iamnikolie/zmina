package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

func (a *app) seatCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "seat",
		Short: "Add, change or remove a role in the crew file, comments kept, checked before it is written",
	}

	c.AddCommand(a.seatAddCommand(), a.seatSetCommand(), a.seatRmCommand())

	return c
}

func (a *app) seatAddCommand() *cobra.Command {
	values := map[string]*string{}

	c := &cobra.Command{
		Use:   "add <role>",
		Short: "Add a role (--about, --dir, --boot, --model, --keep, --args, --expires 3d, --schedule, --prompt, ...)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set := map[string]string{}

			for k, p := range values {
				if cmd.Flags().Changed(k) {
					set[k] = *p
				}
			}

			if v, ok := set["expires"]; ok {
				var err error
				if set["expires"], err = expiry(v); err != nil {
					return err
				}
			}

			return a.editSeat(args[0], sortedPairs(set), func(s *shift.Shift, e *crew.Edit) (string, error) {
				return "added", e.Add(args[0], set)
			})
		},
	}

	for _, k := range crew.SeatKeys {
		values[k] = c.Flags().String(k, "", "the role's "+k)
	}

	return c
}

func (a *app) seatSetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set <role> <key=value...>",
		Short: "Change keys of a role; key= removes the key (keys: " + strings.Join(crew.SeatKeys, ", ") + ")",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			set := map[string]string{}

			for _, kv := range args[1:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("%q: key=value", kv)
				}

				if k == "expires" && v != "" {
					var err error
					if v, err = expiry(v); err != nil {
						return err
					}
				}

				set[k] = v
			}

			return a.editSeat(args[0], sortedPairs(set), func(s *shift.Shift, e *crew.Edit) (string, error) {
				return "changed", e.Set(args[0], set)
			})
		},
	}
}

func (a *app) seatRmCommand() *cobra.Command {
	var force bool

	c := &cobra.Command{
		Use:   "rm <role>",
		Short: "Remove a role that is down and holds no active task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			return a.editSeat(name, "", func(s *shift.Shift, e *crew.Edit) (string, error) {
				if name == s.Crew.Dispatcher {
					return "", errors.New("the dispatcher stays; point dispatcher: at another role first")
				}

				for _, v := range s.Views() {
					if base, _, _ := shift.SplitInstance(v.Role); base != name {
						continue
					}

					if v.State != shift.PresenceDown {
						return "", fmt.Errorf("%s is %s; zmina down %s first", v.Role, v.State, v.Role)
					}

					if len(v.Tasks) > 0 && !force {
						return "", fmt.Errorf("%s holds %s; close or hand them over first (--force removes it anyway)",
							v.Role, strings.Join(v.Tasks, ", "))
					}
				}

				return "removed", e.Remove(name)
			})
		},
	}

	c.Flags().BoolVar(&force, "force", false, "remove it although it holds active tasks")

	return c
}

func (a *app) editSeat(role, detail string, fn func(s *shift.Shift, e *crew.Edit) (string, error)) error {
	return a.withState(func(s *shift.Shift) error {
		e, err := crew.Open(s.Crew.Path)
		if err != nil {
			return err
		}

		did, err := fn(s, e)
		if err != nil {
			return err
		}

		if err := e.Save(); err != nil {
			return err
		}

		s.Log(role, "seat", "", strings.TrimSpace(did+" "+detail))
		fmt.Fprintf(a.out, "%s %s in %s (old file: %s.bak)\n", role, did, s.Crew.Path, s.Crew.Path)

		return nil
	})
}

// expiry turns 3d or 12h into a date and time from now; dates and times are kept as given.
func expiry(v string) (string, error) {
	if d, err := parseAge(v); err == nil {
		return time.Now().Add(d).Format("2006-01-02 15:04"), nil
	}

	if _, err := crew.ParseTime(v); err != nil {
		return "", fmt.Errorf("expires: %w, or a span: 12h, 3d", err)
	}

	return v, nil
}

func sortedPairs(m map[string]string) string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}

	sort.Strings(out)

	return strings.Join(out, " ")
}
