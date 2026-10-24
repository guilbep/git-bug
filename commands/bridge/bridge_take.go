package bridgecmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/commands/execenv"
)

func newBridgeTakeCommand(env *execenv.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "take BUG_ID BRIDGE",
		Short: "Replace an issue's unsent changes with its version on a forge",
		Long: `Replace what git-bug hasn't sent to a bridge's forge yet, for one issue, with
the forge's version: the title, description, comments' texts, status and
labels the forge has now. What changed on both sides is the forge's too.

Nothing is written to the forge, and nothing is lost: git-bug keeps every
version in the issue's history. Comments made in git-bug stay, and the next
sync sends them. The issue's other forges get this version at the next
sync.`,
		Example: `# an edit made in git-bug by mistake, not synced yet: take GitLab's back
git bug bridge take 4f2a1c9 gl

# after linking, keep the forge's version instead of git-bug's
git bug bridge link 4f2a1c9 gl 12
git bug bridge take 4f2a1c9 gl`,
		PreRunE: execenv.LoadBackend(env, execenv.EnsureUser()),
		RunE: execenv.CloseBackend(env, func(cmd *cobra.Command, args []string) error {
			return runBridgeTake(env, args)
		}),
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: bugThenBridge(env),
	}

	return cmd
}

func runBridgeTake(env *execenv.Env, args []string) error {
	results, err := core.Take(context.Background(), env.Backend, args[0], args[1])
	if err != nil {
		return err
	}
	changes, errs := 0, 0
	for r := range results {
		switch r.Import.Event {
		case core.ImportEventError:
			errs++
		case core.ImportEventNothing, core.ImportEventWarning, core.ImportEventRateLimiting:
		default:
			changes++
		}
		if show(r, false) {
			env.Out.Println(r.String())
		}
	}
	if errs > 0 {
		return fmt.Errorf("take finished with %d error(s)", errs)
	}
	// warnings alone, like the issue deleted on the forge, fail nothing, as
	// with bridge sync
	if changes == 0 {
		env.Out.Printf("nothing to take for %s from bridge %s\n", args[0], args[1])
	}
	return nil
}
