package bridgecmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/commands/execenv"
)

func newBridgeUnlinkCommand(env *execenv.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unlink BUG_ID BRIDGE",
		Short: "Stop syncing a git-bug issue with a forge",
		Long: `Stop syncing a git-bug issue with its issue on a bridge's forge.

Both keep what they have: nothing is deleted, here or on the forge. From now
on, sync doesn't send the git-bug issue to that forge, nor create it there
again, nor pull the forge issue, which can be linked to another git-bug
issue with "git bug bridge link". The issue's other bridges still sync it.

This can't be undone: once unlinked from a forge, a git-bug issue can't be
linked to an issue of that forge again.`,
		Example: `git bug bridge unlink 4f2a1c9 gl`,
		PreRunE: execenv.LoadBackend(env, execenv.EnsureUser()),
		RunE: execenv.CloseBackend(env, func(cmd *cobra.Command, args []string) error {
			return runBridgeUnlink(env, args)
		}),
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: bugThenBridge(env),
	}

	return cmd
}

func runBridgeUnlink(env *execenv.Env, args []string) error {
	if err := core.Unlink(context.Background(), env.Backend, args[0], args[1]); err != nil {
		return err
	}
	env.Out.Printf("%s is no longer synced with bridge %s\n", args[0], args[1])
	return nil
}
