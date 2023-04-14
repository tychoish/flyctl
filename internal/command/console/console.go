package console

import (
	"context"
	"errors"
	"fmt"

	"github.com/samber/lo"
	"github.com/spf13/cobra"

	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/client"
	"github.com/superfly/flyctl/flaps"
	"github.com/superfly/flyctl/internal/appconfig"
	"github.com/superfly/flyctl/internal/command"
	"github.com/superfly/flyctl/internal/command/ssh"
	"github.com/superfly/flyctl/internal/flag"
	"github.com/superfly/flyctl/internal/prompt"
	"github.com/superfly/flyctl/iostreams"
)

func New() *cobra.Command {
	const (
		usage = "console <machine id>"
		short = ""
		long  = "\n" // TODO
	)
	cmd := command.New(usage, short, long, runConsole, command.RequireSession, command.RequireAppName)

	cmd.Args = cobra.RangeArgs(0, 1)
	flag.Add(
		cmd,
		flag.App(),
		flag.AppConfig(),
		flag.String{
			Name:        "user",
			Shorthand:   "u",
			Description: "Unix username to connect as",
			Default:     ssh.DefaultSshUsername,
		},
		flag.Bool{
			Name:        "select",
			Shorthand:   "s",
			Description: "Select from a list of machines",
			Default:     false,
		},
	)

	return cmd
}

func runConsole(ctx context.Context) error {
	appName := appconfig.NameFromContext(ctx)
	apiClient := client.FromContext(ctx).API()

	app, err := apiClient.GetAppCompact(ctx, appName)
	if err != nil {
		return fmt.Errorf("failed to get app: %w", err)
	}

	if app.PlatformVersion != "machines" {
		return errors.New("console is only supported for the machines platform")
	}

	flapsClient, err := flaps.New(ctx, app)
	if err != nil {
		return fmt.Errorf("failed to create flaps client: %w", err)
	}
	ctx = flaps.NewContext(ctx, flapsClient)

	appConfig := appconfig.ConfigFromContext(ctx)
	if appConfig == nil {
		appConfig, err = appconfig.FromRemoteApp(ctx, appName)
		if err != nil {
			return fmt.Errorf("failed to fetch app config from backend: %w", err)
		}
	}

	if err, extraInfo := appConfig.ValidateForMachinesPlatform(ctx); err != nil {
		fmt.Fprintln(iostreams.FromContext(ctx).ErrOut, extraInfo)
		return err
	}

	machine, err := selectMachine(ctx)
	if err != nil {
		return err
	}

	_, dialer, err := ssh.BringUpAgent(ctx, apiClient, app, false)
	if err != nil {
		return err
	}

	params := &ssh.ConnectParams{
		Ctx:            ctx,
		Org:            app.Organization,
		Dialer:         dialer,
		Username:       flag.GetString(ctx, "user"),
		DisableSpinner: false,
	}
	sshClient, err := ssh.Connect(params, machine.PrivateIP)
	if err != nil {
		return err
	}

	return ssh.Console(ctx, sshClient, appConfig.ConsoleCommand, true)
}

func selectMachine(ctx context.Context) (*api.Machine, error) {
	if flag.GetBool(ctx, "select") {
		return promptForMachine(ctx)
	} else if len(flag.Args(ctx)) == 1 {
		return getMachineByID(ctx)
	} else {
		return nil, errors.New("a machine ID must be provided unless -s/--select is used")
	}
}

func promptForMachine(ctx context.Context) (*api.Machine, error) {
	if len(flag.Args(ctx)) != 0 {
		return nil, errors.New("machine IDs can't be used with -s/--select")
	}

	flapsClient := flaps.FromContext(ctx)
	machines, err := flapsClient.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	machines = lo.Filter(machines, func(machine *api.Machine, _ int) bool {
		return machine.State == api.MachineStateStarted && !machine.IsFlyAppsReleaseCommand()
	})
	if len(machines) == 0 {
		return nil, errors.New("no machines are available")
	}

	options := []string{}
	for _, machine := range machines {
		options = append(options, fmt.Sprintf("%s: %s %s %s", machine.Region, machine.ID, machine.PrivateIP, machine.Name))
	}

	index := 0
	if err := prompt.Select(ctx, &index, "Select a machine:", "", options...); err != nil {
		return nil, fmt.Errorf("failed to prompt for a machine: %w", err)
	}
	return machines[index], nil
}

func getMachineByID(ctx context.Context) (*api.Machine, error) {
	flapsClient := flaps.FromContext(ctx)
	machineID := flag.FirstArg(ctx)
	machine, err := flapsClient.Get(ctx, machineID)
	if err != nil {
		return nil, err
	}
	if machine.State != api.MachineStateStarted {
		return nil, fmt.Errorf("machine %s is not started", machineID)
	}
	if machine.IsFlyAppsReleaseCommand() {
		return nil, fmt.Errorf("machine %s is a release command machine", machineID)
	}

	return machine, nil
}
