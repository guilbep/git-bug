package bridgecmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/git-bug/git-bug/bridge/core"
	bugcmd "github.com/git-bug/git-bug/commands/bug"
	"github.com/git-bug/git-bug/commands/completion"
	"github.com/git-bug/git-bug/commands/execenv"
)

func newBridgeLinkCommand(env *execenv.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "link BUG_ID BRIDGE ISSUE",
		Short: "Sync a git-bug issue with an issue already on a forge",
		Long: `Sync a git-bug issue with an issue already on a bridge's forge, instead of
creating it there again. ISSUE is the forge issue's number or URL.

Use it when the same issue is on both sides, for example copied by hand, or
by a sync from another repository. Nothing is written to the forge now. The
next sync makes them one:

  - the title, description and status are git-bug's: they are sent as
    changes not sent yet (to keep the forge's instead, run
    "git bug bridge take" right after linking);
  - the comments and labels of each side are added to the other.

It refuses a forge issue already synced with another git-bug issue, and a
git-bug issue that has, or had, an issue on that forge.`,
		Example: `git bug bridge link 4f2a1c9 gl 12
git bug bridge link 4f2a1c9 gh https://github.com/example-owner/example-repo/issues/12

# what the next sync would send, then send it
git bug bridge sync --issue 4f2a1c9 --dry-run
git bug bridge sync --issue 4f2a1c9`,
		PreRunE: execenv.LoadBackend(env, execenv.EnsureUser()),
		RunE: execenv.CloseBackend(env, func(cmd *cobra.Command, args []string) error {
			return runBridgeLink(env, args)
		}),
		Args:              cobra.ExactArgs(3),
		ValidArgsFunction: bugThenBridge(env),
	}

	return cmd
}

func runBridgeLink(env *execenv.Env, args []string) error {
	issue, err := core.Link(context.Background(), env.Backend, args[0], args[1], args[2])
	if err != nil {
		return err
	}
	env.Out.Printf("%s is now synced with %s, by the next \"git bug bridge sync\"\n", args[0], issue.URL)
	return nil
}

// bugThenBridge completes a bug id, then a bridge name.
func bugThenBridge(env *execenv.Env) completion.ValidArgsFunction {
	bugs, bridges := bugcmd.BugCompletion(env), completion.Bridge(env)
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			return bugs(cmd, args, toComplete)
		case 1:
			return bridges(cmd, args, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}
