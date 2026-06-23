package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

// bucketCmd is the `pbrainctl server bucket ...` parent. Wraps the
// MinIO admin verbs the operator needs when carving out a new
// per-binding bucket. v3.3.
func bucketCmd() *cobra.Command {
	c := &cobra.Command{Use: "bucket", Short: "MinIO bucket admin (create, list)"}
	c.AddCommand(bucketCreateCmd(), bucketListCmd())
	return c
}

func bucketCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create [name]",
		Short: "Create a MinIO bucket using the daemon's credentials (idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			ctx, cancel := signalCancel(cmd.Context())
			defer cancel()
			mb, err := openMinIOForOps(cmd)
			if err != nil {
				return err
			}
			if err := mb.CreateBucket(ctx, name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "bucket %q ready\n", name)
			return nil
		},
	}
	opsCommonFlags(c)
	return c
}

func bucketListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List buckets visible to the daemon's MinIO credentials",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCancel(cmd.Context())
			defer cancel()
			mb, err := openMinIOForOps(cmd)
			if err != nil {
				return err
			}
			buckets, err := mb.ListBuckets(ctx)
			if err != nil {
				return err
			}
			for _, b := range buckets {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n",
					b.Name, b.CreationDate.UTC().Format(time.RFC3339))
			}
			return nil
		},
	}
	opsCommonFlags(c)
	return c
}

// openMinIOForOps boots the daemon's MinIO backend from server.toml
// for use by operator CLI subcommands. Refuses when the daemon is not
// configured for the minio backend — there are no creds to use
// otherwise.
func openMinIOForOps(cmd *cobra.Command) (*pbserver.MinIOBackend, error) {
	cfg, err := pbserver.LoadServerConfig(resolveConfigDir(cmd))
	if err != nil {
		return nil, fmt.Errorf("load server config: %w", err)
	}
	if cfg.Storage.Backend != "minio" {
		return nil, errors.New("server.toml has [storage] backend != \"minio\" — no MinIO credentials to use")
	}
	mb, err := pbserver.NewMinIOBackend(pbserver.MinIOOptions{
		Endpoint:  cfg.Storage.MinIOEndpoint,
		Bucket:    cfg.Storage.MinIOBucket,
		AccessKey: cfg.Storage.MinIOAccessKey,
		SecretKey: cfg.Storage.MinIOSecretKey,
		UseSSL:    cfg.Storage.MinIOUseSSL,
		DataDir:   resolveDataDir(cmd),
	})
	if err != nil {
		return nil, fmt.Errorf("minio backend: %w", err)
	}
	return mb, nil
}
