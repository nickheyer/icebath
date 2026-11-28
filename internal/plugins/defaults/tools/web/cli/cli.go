// The web command: the services that search the web and read pages, how each stands, and what each is used for, chosen as /web does
package cli

import (
	"cmp"
	"errors"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	picked "github.com/nickheyer/icebath/internal/profile"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
	webv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/web/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/web/v1/webv1connect"
)

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

// What --for takes, and what each asks the server for
var uses = map[string]webv1.Use{"both": webv1.Use_USE_BOTH, "searches": webv1.Use_USE_SEARCH, "pages": webv1.Use_USE_PAGES}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := webv1connect.NewWebServiceClient(conn)
	var profile string
	root := &cobra.Command{
		Use:     "web",
		GroupID: cli.GroupAbilities,
		Short:   "Show which services search the web and read pages",
		Long: "Lists the services that search the web and read pages for the agent, how each stands and what answers now. use chooses a " +
			"service for searches and pages, or for one of them, and automatic leaves it to the keys you have, as /web does.",
		Example: "  icebath web\n  icebath web use brave-free --for searches\n  icebath web use exa --plan free\n  icebath web automatic",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return list(cmd, service, profile) },
	}
	root.PersistentFlags().StringVar(&profile, "profile", "", "icebath profile to act for, instead of the one in use")
	var use, plan string
	choose := &cobra.Command{
		Use:   "use <service>",
		Short: "Choose a service for searches and pages, or for one of them",
		Long: "Chooses the service for searches and pages, or with --for searches or --for pages for one of them, clearing what the choice " +
			"replaces. --plan sets its plan: free, paid, or auto for the plan its key decides.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request := &webv1.ChooseRequest{Profile: named(cmd, profile), Backend: strings.ToLower(args[0]), Plan: plan}
			if use != "" {
				chosen, ok := uses[use]
				if !ok {
					return errors.New("--for takes both, searches or pages, not " + use + ".")
				}
				request.Use = chosen
			} else if plan == "" {
				request.Use = webv1.Use_USE_BOTH
			}
			if plan != "" && plan != webconfig.Free && plan != webconfig.Paid && plan != webconfig.Auto {
				return errors.New("--plan takes free, paid or auto, not " + plan + ".")
			}
			return say(cmd, service, request)
		},
	}
	choose.Flags().StringVar(&use, "for", "", "What the service is used for: both, searches or pages")
	choose.Flags().StringVar(&plan, "plan", "", "Plan of the service: free, paid or auto")
	root.AddCommand(choose, &cobra.Command{
		Use:   "automatic",
		Short: "Leave web search to the services your keys allow",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return say(cmd, service, &webv1.ChooseRequest{Profile: named(cmd, profile)})
		},
	})
	return []*cobra.Command{root}
}

// Chooses as asked and prints what is in use for what now
func say(cmd *cobra.Command, service webv1connect.WebServiceClient, request *webv1.ChooseRequest) error {
	c := cli.NewConsole(cmd)
	response, err := service.Choose(cmd.Context(), request)
	if err != nil {
		return err
	}
	c.Say(c.Tick() + " " + response.GetNote())
	return nil
}

// Lists the services with their states and what answers now
func list(cmd *cobra.Command, service webv1connect.WebServiceClient, profile string) error {
	c := cli.NewConsole(cmd)
	listed, err := service.List(cmd.Context(), &webv1.ListRequest{Profile: named(cmd, profile)})
	if err != nil {
		return err
	}
	automatic := listed.GetBackend() == "" && listed.GetSearchBackend() == "" && listed.GetExtractBackend() == ""
	mark := func(on bool) string {
		if on {
			return c.Paint("●", cli.Green)
		}
		return ""
	}
	rows := [][]string{{mark(automatic), "Automatic", "", "The first service you have an API key for, else a free one"}}
	for _, s := range listed.GetBackends() {
		chosen := listed.GetBackend() == s.GetName() || listed.GetSearchBackend() == s.GetName() || listed.GetExtractBackend() == s.GetName()
		state := s.GetState()
		switch {
		case s.GetSearching() || s.GetReading():
			state = c.Paint(state, cli.Green)
		case !s.GetUsable():
			state = c.Paint(state, cli.Yellow)
		}
		rows = append(rows, []string{mark(chosen), s.GetTitle(), s.GetName(), state})
	}
	c.Table([]string{"", "Service", "Name", "State"}, rows)
	if serving := listed.GetServing(); serving != "" {
		c.Blank()
		c.Say(serving)
	}
	if !listed.GetAvailable() {
		c.Blank()
		c.Warning("The agent can't search the web until a service is ready. icebath web use <service> chooses one.")
	}
	return nil
}

// Profile a command acts for: the one its --profile names, else the one -p picked for icebath, else the one in use
func named(cmd *cobra.Command, flag string) string {
	return cmp.Or(flag, picked.Picked(cmd.Context()))
}
