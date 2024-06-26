package cmd

import (
	"fmt"
	"github.com/ozkatz/cloudzip/pkg/mount/dav"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/ozkatz/cloudzip/pkg/mount"
	"github.com/ozkatz/cloudzip/pkg/mount/nfs"
)

const (
	cacheDirEnvironmentVariableName = "CLOUDZIP_CACHE_DIR"
)

func dieWithCallback(toAddr, fstring string, args ...interface{}) {
	errMessage := fmt.Sprintf(fstring, args...)
	var err error
	if toAddr != "" {
		err = sendCallback(toAddr, mountServerStatusError, errMessage)
	}
	if err != nil {
		errMessage += fmt.Sprintf(" (could not notify callback address %s: %v)", toAddr, err)
	}
	die(errMessage)
}

func sendCallback(toAddr string, className mountServerStatus, msg string) error {
	conn, err := net.Dial("tcp4", toAddr)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(conn, "%s=%s\n", className, msg)
	if err != nil {
		return err
	}
	return conn.Close()
}

func serverLogging(logFile string) (*slog.Logger, error) {
	writer := io.Discard
	var err error
	switch logFile {
	case "":
		writer = os.Stdout
	default:
		writer, err = os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0755)
		if err != nil {
			return nil, err
		}
	}
	opts := &slog.HandlerOptions{AddSource: false, Level: slog.LevelInfo}
	if os.Getenv("CLOUDZIP_LOGGING") == "DEBUG" {
		opts.Level = slog.LevelDebug
	}
	handler := slog.NewJSONHandler(writer, opts)
	return slog.New(handler), nil
}

var mountServerCmd = &cobra.Command{
	Use:    "mount-server",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		ctx := cmd.Context()
		remoteFile := args[0]
		cacheDir, err := cmd.Flags().GetString("cache-dir")
		if err != nil {
			die("could not parse command flags: %v\n", err)
		}
		listenAddr, err := cmd.Flags().GetString("listen")
		if err != nil {
			die("could not parse command flags: %v\n", err)
		}
		callbackAddr, err := cmd.Flags().GetString("callback-addr")
		if err != nil {
			die("could not parse command flags: %v\n", err)
		}
		logFile, err := cmd.Flags().GetString("log")
		if err != nil {
			die("could not parse command flags: %v\n", err)
		}
		protocol, err := cmd.Flags().GetString("protocol")
		if err != nil {
			die("could not parse command flags: %v\n", err)
		}

		// setup logging
		logger, err := serverLogging(logFile)
		if err != nil {
			if err != nil {
				dieWithCallback(callbackAddr, "could not open log file %s: %v\n", logFile, err)
			}
		}

		logger.InfoContext(
			cmd.Context(),
			"starting mount server",
			"cache_dir", cacheDir,
			"listen_addr", listenAddr,
			"callback_addr", callbackAddr,
			"log_file", logFile,
			"protocol", protocol)

		// handle cache dir
		if cacheDir == "" {
			cacheDir = os.Getenv(cacheDirEnvironmentVariableName)
		}
		if cacheDir == "" {
			cacheDir = filepath.Join(os.TempDir(), "cz-mount-cache", uuid.Must(uuid.NewV7()).String())
			// auto generated cache dir. Let's try and remove it when done:
			defer func() {
				err := os.RemoveAll(cacheDir)
				if err != nil {
					dieWithCallback(callbackAddr, "could not clear cache dir at %s: %v\n", cacheDir, err)
				}
			}()
		}
		dirExists, err := isDir(cacheDir)
		if err != nil {
			dieWithCallback(callbackAddr, "could not check if cache directory '%s' exists: %v\n", cacheDir, err)
		}

		if !dirExists {
			err := os.MkdirAll(cacheDir, 0755)
			if err != nil {
				dieWithCallback(callbackAddr, "could not create local cache directory: %v\n", err)
			}
		}

		// bind to listen address
		listener, err := net.Listen("tcp4", listenAddr)
		if err != nil {
			dieWithCallback(callbackAddr, "could not listen on %s: %v\n", listenAddr, err)
		}
		boundAddr := listener.Addr()

		// build index for remote archive
		tree, err := mount.BuildZipTree(ctx, logger, cacheDir, remoteFile, map[string]interface{}{
			"listen_addr": boundAddr,
			"protocol":    protocol,
			"version":     CloudZipVersion,
			"logfile":     logFile,
		})
		if err != nil {
			dieWithCallback(callbackAddr, "could not create filesystem: %v\n", err)
		}

		// setup signal handling
		ctx, cancelFn := signal.NotifyContext(ctx, os.Interrupt) // SIGTERM
		defer cancelFn()

		if protocol == "nfs" {
			handler := nfs.NewHandler(ctx, tree, &nfs.Options{
				Logger:          logger,
				HandleCacheSize: nfs.DefaultHandleCacheSize,
			})
			go func() {
				err = nfs.Serve(ctx, listener, handler)
				if err != nil {
					dieWithCallback(callbackAddr,
						"could not serve NFS server on listener: %s: %v\n",
						boundAddr, err)
				}
			}()
		} else if protocol == "webdav" {
			go func() {
				err = dav.Serve(listener, tree, logger)
				if err != nil {
					dieWithCallback(callbackAddr,
						"could not serve WebDav server on listener: %s: %v\n",
						boundAddr, err)
				}
			}()
		} else {
			dieWithCallback(callbackAddr,
				"unknown protocol: '%s'. Supported types are 'nfs' and 'webdav'", protocol)
		}

		if callbackAddr != "" {
			err = sendCallback(callbackAddr, mountServerStatusSuccess, boundAddr.String())
			if err != nil {
				die("could not send listen address back to caller: %v\n", err)
			}
		}

		logger.InfoContext(cmd.Context(),
			"mount server started successfully",
			"bound_addr", boundAddr.String(), "protocol", protocol)
		<-ctx.Done()
	},
}

func init() {
	mountServerCmd.Flags().String("cache-dir", "", "directory to cache read files in")
	mountServerCmd.Flags().StringP("listen", "l", MountServerBindAddress, "address to listen on")
	mountServerCmd.Flags().String("protocol", "nfs", "protocol to use (nfs | webdav)")
	mountServerCmd.Flags().String("log", "", "optional log file to write to")
	mountServerCmd.Flags().String("callback-addr", "", "callback address to report back to")
	rootCmd.AddCommand(mountServerCmd)
}
