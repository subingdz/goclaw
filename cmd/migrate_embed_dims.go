package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

// migrateEmbedDimsCmd changes the width of every pgvector column in the database.
//
// The schema — not a config key — is the source of truth for "what width are we", so this
// command is one of only two ways to change it. The other is the operator endpoint in
// internal/http, and both call pg.Redimension: the refusal conditions and the DDL are not
// duplicated here on purpose.
func migrateEmbedDimsCmd() *cobra.Command {
	var (
		target   int
		strategy string
		force    bool
		dryRun   bool
	)

	cmd := &cobra.Command{
		Use:   "embed-dims",
		Short: "Re-dimension all pgvector columns to a new width",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEmbedDims(cmd.Context(), target, strategy, force, dryRun)
		},
	}
	cmd.Flags().IntVar(&target, "to", 0, "required target dimension (1..2000)")
	cmd.Flags().StringVar(&strategy, "strategy", "reembed", "how to treat existing vectors: reembed (clear and let the backfill redo them)")
	cmd.Flags().BoolVar(&force, "force", false, "required when columns already hold vectors")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without touching the database")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func runEmbedDims(parent context.Context, target int, strategy string, force, dryRun bool) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	switch strategy {
	case "reembed":
	case "truncate":
		// Stated as unimplemented rather than attempted: truncating a vector and
		// re-normalising it is only equivalent to what the provider itself does for
		// some models, and we have not verified that here.
		return fmt.Errorf("--strategy truncate is not implemented in this build; use reembed")
	default:
		return fmt.Errorf("unknown --strategy %q (want reembed)", strategy)
	}

	// Checked before the DSN is even resolved: an impossible width should be reported as an
	// impossible width, not as "GOCLAW_POSTGRES_DSN is not set".
	if err := pg.ValidateTarget(target); err != nil {
		return err
	}

	dsn, err := resolveDSN()
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer db.Close()

	report, err := pg.Redimension(ctx, db, target, force, dryRun)
	if report != nil {
		printRedimensionReport(report, dryRun)
	}
	if err != nil {
		if errors.Is(err, pg.ErrVectorsPresent) {
			return fmt.Errorf("%w\n"+
				"Re-run with --force --strategy reembed to clear them and let the memory backfill\n"+
				"regenerate them (that costs embedding API calls proportional to the stored chunks).", err)
		}
		return err
	}
	if dryRun {
		fmt.Println("dry-run: no changes applied")
		return nil
	}
	fmt.Printf("\ndone. Start the gateway so the memory backfill regenerates vectors at %d dimensions.\n", target)
	return nil
}

func printRedimensionReport(report *pg.RedimensionReport, dryRun bool) {
	fmt.Printf("found %d vector column(s):\n", len(report.Columns))
	for _, c := range report.Columns {
		status := fmt.Sprintf("  %-34s vector(%-6d) -> vector(%-6d) %2d index(es)  %s",
			c.Table+"."+c.Column, c.From, c.To, len(c.Indexes), c.Action)
		switch {
		case c.Err != nil && !c.Applied:
			status += "  <-- " + c.Err.Error()
		case c.Applied:
			status = status + "  applied"
		}
		fmt.Println(status)
	}
	fmt.Printf("\n%d row(s) currently hold vectors; %d column(s) would move to %d dimensions\n",
		report.EmbeddedRows, countRedimension(report), report.Target)
	if report.NeedsForce && !dryRun {
		fmt.Println("note: --force was accepted, so those vectors have been cleared")
	}
}

func countRedimension(report *pg.RedimensionReport) int {
	n := 0
	for _, c := range report.Columns {
		if c.Action == pg.ActionRedimension {
			n++
		}
	}
	return n
}
