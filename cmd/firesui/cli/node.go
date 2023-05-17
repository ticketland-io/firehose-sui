package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/streamingfast/bstream"
	"github.com/streamingfast/bstream/blockstream"
	"github.com/streamingfast/dlauncher/launcher"
	"github.com/streamingfast/logging"
	nodeManager "github.com/streamingfast/node-manager"
	nodeManagerApp "github.com/streamingfast/node-manager/app/node_manager2"
	"github.com/streamingfast/node-manager/metrics"
	"github.com/streamingfast/node-manager/operator"
	pbbstream "github.com/streamingfast/pbgo/sf/bstream/v1"
	pbheadinfo "github.com/streamingfast/pbgo/sf/headinfo/v1"
	"github.com/ticketland-io/firehose-sui/nodemanager"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

var nodeLogger, nodeTracer = logging.PackageLogger("node", "github.com/ticketland-io/firehose-sui/node")
var nodeSuiChainLogger, _ = logging.PackageLogger("node.sui", "github.com/ticketland-io/firehose-sui/node/sui", DefaultLevelInfo)

var readerLogger, readerTracer = logging.PackageLogger("reader", "github.com/ticketland-io/firehose-sui/reader")
var readerSuiChainLogger, _ = logging.PackageLogger("reader.sui", "github.com/ticketland-io/firehose-sui/reader/sui", DefaultLevelInfo)

func registerCommonNodeFlags(cmd *cobra.Command, flagPrefix string, managerAPIAddr string) {
	cmd.Flags().String(flagPrefix+"path", ChainExecutableName, FlagDescription(`
		Process that will be invoked to sync the chain, can be a full path or just the binary's name, in which case the binary is
		searched for paths listed by the PATH environment variable (following operating system rules around PATH handling).
	`))
	cmd.Flags().String(flagPrefix+"data-dir", "{data-dir}/{node-role}/data", "Directory for node data ({node-role} is either reader, peering or dev-miner)")
	cmd.Flags().String(flagPrefix+"config-file", "sui.yaml", "Path where to find the node's config file that is passed directly to the executable")
	cmd.Flags().String(flagPrefix+"genesis-file", "", "Path where to find the node's genesis.blob file for the network, if defined, going to be copied inside node data directory automatically and '{genesis-file}' will be replaced in config automatically to this value.")
	cmd.Flags().String(flagPrefix+"waypoint-file", "", "Path where to find the node's waypoint.txt file for the network, if defined, going to be copied inside node data directory automatically and '{waypoint-file}' will be replaced in config automatically to this value.")
	cmd.Flags().String(flagPrefix+"validator-identity-file", "", "Path where to find the node's validator-identity.yaml file for the network, if defined, going to be copied inside node data directory automatically and '{validator-identity-file}' will be replaced in config automatically to this value.")
	cmd.Flags().String(flagPrefix+"vfn-identity-file", "", "Path where to find the node's vfn-identity.yaml file for the network, if defined, going to be copied inside node data directory automatically and '{vfn-identity-file}' will be replaced in config automatically to this value.")
	cmd.Flags().Bool(flagPrefix+"debug-firehose-logs", false, "[DEV] Prints Firehose instrumentation logs to standard output, should be use for debugging purposes only")
	cmd.Flags().Bool(flagPrefix+"log-to-zap", true, FlagDescription(`
		When sets to 'true', all standard error output emitted by the invoked process defined via '%s'
		is intercepted, split line by line and each line is then transformed and logged through the Firehose stack
		logging system. The transformation extracts the level and remove the timestamps creating a 'sanitized' version
		of the logs emitted by the blockchain's managed client process. If this is not desirable, disabled the flag
		and all the invoked process standard error will be redirect to 'firesui' standard's output.
	`, flagPrefix+"path"))
	cmd.Flags().String(flagPrefix+"manager-api-addr", managerAPIAddr, "Sui node manager API address")
	cmd.Flags().Duration(flagPrefix+"readiness-max-latency", 30*time.Second, "Determine the maximum head block latency at which the instance will be determined healthy. Some chains have more regular block production than others.")
	cmd.Flags().String(flagPrefix+"arguments", "", "If not empty, overrides the list of default node arguments (computed from node type and role). Start with '+' to append to default args instead of replacing. ")
}

func registerNode(kind string, extraFlagRegistration func(cmd *cobra.Command) error, managerAPIaddr string) {
	if kind != "reader" {
		panic(fmt.Errorf("invalid kind value, must be either 'reader', got %q", kind))
	}

	app := fmt.Sprintf("%s-node", kind)
	flagPrefix := fmt.Sprintf("%s-", app)

	launcher.RegisterApp(rootLog, &launcher.AppDef{
		ID:          app,
		Title:       fmt.Sprintf("Sui Node (%s)", kind),
		Description: fmt.Sprintf("Sui %s node with built-in operational manager", kind),
		RegisterFlags: func(cmd *cobra.Command) error {
			registerCommonNodeFlags(cmd, flagPrefix, managerAPIaddr)
			extraFlagRegistration(cmd)
			return nil
		},
		InitFunc: func(runtime *launcher.Runtime) error {
			return nil
		},
		FactoryFunc: nodeFactoryFunc(flagPrefix, kind),
	})
}

func nodeFactoryFunc(flagPrefix, kind string) func(*launcher.Runtime) (launcher.App, error) {
	return func(runtime *launcher.Runtime) (launcher.App, error) {
		var appLogger *zap.Logger
		var appTracer logging.Tracer
		var supervisedProcessLogger *zap.Logger

		switch kind {
		case "node":
			appLogger = nodeLogger
			appTracer = nodeTracer
			supervisedProcessLogger = nodeSuiChainLogger
		case "reader":
			appLogger = readerLogger
			appTracer = readerTracer
			supervisedProcessLogger = readerSuiChainLogger
		default:
			panic(fmt.Errorf("unknown node kind %q", kind))
		}

		sfDataDir := runtime.AbsDataDir

		nodePath := viper.GetString(flagPrefix + "path")
		nodeDataDir := replaceNodeRole(kind, mustReplaceDataDir(sfDataDir, viper.GetString(flagPrefix+"data-dir")))
		nodeConfigFile := mustReplaceDataDir(sfDataDir, viper.GetString(flagPrefix+"config-file"))
		nodeGenesisFile := mustReplaceDataDir(sfDataDir, viper.GetString(flagPrefix+"genesis-file"))
		nodeWaypointFile := mustReplaceDataDir(sfDataDir, viper.GetString(flagPrefix+"waypoint-file"))
		nodeValidatorIdentityFile := mustReplaceDataDir(sfDataDir, viper.GetString(flagPrefix+"validator-identity-file"))
		nodeVFNIdentityFile := mustReplaceDataDir(sfDataDir, viper.GetString(flagPrefix+"vfn-identity-file"))
		resolvedNodeConfigFile := filepath.Join(nodeDataDir, "node.yaml")

		readinessMaxLatency := viper.GetDuration(flagPrefix + "readiness-max-latency")
		debugFirehoseLogs := viper.GetBool(flagPrefix + "debug-firehose-logs")
		logToZap := viper.GetBool(flagPrefix + "log-to-zap")
		shutdownDelay := viper.GetDuration("common-system-shutdown-signal-delay") // we reuse this global value
		httpAddr := viper.GetString(flagPrefix + "manager-api-addr")

		arguments := viper.GetString(flagPrefix + "arguments")
		nodeArguments, err := buildNodeArguments(
			appLogger,
			nodeDataDir,
			resolvedNodeConfigFile,
			kind,
			arguments,
		)
		if err != nil {
			return nil, fmt.Errorf("cannot build node bootstrap arguments")
		}
		metricsAndReadinessManager := buildMetricsAndReadinessManager(flagPrefix, readinessMaxLatency)

		readerWorkindDir := mustReplaceDataDir(sfDataDir, viper.GetString("reader-node-working-dir"))
		syncStateFile := filepath.Join(readerWorkindDir, "sync_state.json")
		syncState, err := readNodeSyncState(appLogger, syncStateFile)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("read node sync state: %w", err)
			}

			initialStartBlock := bstream.GetProtocolFirstStreamableBlock

			appLogger.Info("overriding initial node sync state based to be first streamable block", zap.Uint64("starting_block", initialStartBlock))
			syncState = &readerNodeSyncState{BlockNum: initialStartBlock}
		}

		appLogger.Info("inital sync state used to restart node", zap.Reflect("state", syncState))

		superviser := nodemanager.NewSuperviser(
			nodePath,
			nodeArguments,
			nodeDataDir,
			debugFirehoseLogs,
			logToZap,
			syncState.BlockNum,
			appLogger,
			supervisedProcessLogger,
		)

		bootstrapper := &bootstrapper{
			nodeDataDir:               nodeDataDir,
			nodeConfigFile:            nodeConfigFile,
			resolvedNodeConfigFile:    resolvedNodeConfigFile,
			nodeGenesisFile:           nodeGenesisFile,
			nodeWaypointFile:          nodeWaypointFile,
			nodeValidatorIdentityFile: nodeValidatorIdentityFile,
			nodeVFNIdentityFile:       nodeVFNIdentityFile,
			logger:                    appLogger,
		}

		chainOperator, err := operator.New(
			appLogger,
			superviser,
			metricsAndReadinessManager,
			&operator.Options{
				ShutdownDelay:              shutdownDelay,
				EnableSupervisorMonitoring: true,
				Bootstrapper:               bootstrapper,
			})
		if err != nil {
			return nil, fmt.Errorf("unable to create chain operator: %w", err)
		}

		if kind != "reader" {
			return nodeManagerApp.New(&nodeManagerApp.Config{
				HTTPAddr: httpAddr,
			}, &nodeManagerApp.Modules{
				Operator:                   chainOperator,
				MetricsAndReadinessManager: metricsAndReadinessManager,
			}, appLogger), nil
		}

		blockStreamServer := blockstream.NewUnmanagedServer(blockstream.ServerOptionWithLogger(appLogger))
		oneBlocksStoreURL := mustReplaceDataDir(sfDataDir, viper.GetString("common-one-block-store-url"))
		gprcListenAdrr := viper.GetString("reader-node-grpc-listen-addr")
		batchStartBlockNum := viper.GetUint64("reader-node-start-block-num")
		batchStopBlockNum := viper.GetUint64("reader-node-stop-block-num")
		oneBlockFileSuffix := viper.GetString("reader-node-one-block-suffix")
		blocksChanCapacity := viper.GetInt("reader-node-blocks-chan-capacity")

		readerPlugin, err := getReaderLogPlugin(
			blockStreamServer,
			oneBlocksStoreURL,
			readerWorkindDir,
			batchStartBlockNum,
			batchStopBlockNum,
			blocksChanCapacity,
			oneBlockFileSuffix,
			chainOperator.Shutdown,
			func(lastBlockSeen uint64) {
				superviser.SetLastBlockSeen(lastBlockSeen)
			},
			metricsAndReadinessManager,
			appLogger,
			appTracer,
		)
		if err != nil {
			return nil, fmt.Errorf("new reader plugin: %w", err)
		}

		superviser.RegisterLogPlugin(readerPlugin)

		return nodeManagerApp.New(&nodeManagerApp.Config{
			HTTPAddr: httpAddr,
			GRPCAddr: gprcListenAdrr,
		}, &nodeManagerApp.Modules{
			Operator:                   chainOperator,
			MindreaderPlugin:           readerPlugin,
			MetricsAndReadinessManager: metricsAndReadinessManager,
			RegisterGRPCService: func(registrar grpc.ServiceRegistrar) error {
				pbheadinfo.RegisterHeadInfoServer(registrar, blockStreamServer)
				pbbstream.RegisterBlockStreamServer(registrar, blockStreamServer)

				return nil
			},
		}, appLogger), nil
	}
}

type nodeArgsByRole map[string]string

func buildNodeArguments(logger *zap.Logger, nodeDataDir, nodeConfigFile, nodeRole string, args string) ([]string, error) {
	resolvedNodeConfigFile, err := filepath.Abs(nodeConfigFile)
	if err != nil {
		logger.Warn("unable to make path absolute", zap.String("path", nodeConfigFile), zap.Error(err))
		resolvedNodeConfigFile = nodeConfigFile
	}

	typeRoles := nodeArgsByRole{
		"reader": fmt.Sprintf("--config %s {extra-arg}", resolvedNodeConfigFile),
	}

	argsString, ok := typeRoles[nodeRole]
	if !ok {
		return nil, fmt.Errorf("invalid node role: %s", nodeRole)
	}

	if strings.HasPrefix(args, "+") {
		argsString = strings.Replace(argsString, "{extra-arg}", args[1:], -1)
	} else if args == "" {
		argsString = strings.Replace(argsString, "{extra-arg}", "", -1)
	} else {
		argsString = args
	}

	argsString = strings.Replace(argsString, "{node-data-dir}", nodeDataDir, -1)
	argsSlice := strings.Fields(argsString)
	return argsSlice, nil
}

func buildMetricsAndReadinessManager(name string, maxLatency time.Duration) *nodeManager.MetricsAndReadinessManager {
	headBlockTimeDrift := metrics.NewHeadBlockTimeDrift(name)
	headBlockNumber := metrics.NewHeadBlockNumber(name)
	appReadiness := metrics.NewAppReadiness(name)

	metricsAndReadinessManager := nodeManager.NewMetricsAndReadinessManager(
		headBlockTimeDrift,
		headBlockNumber,
		appReadiness,
		maxLatency,
	)

	return metricsAndReadinessManager
}

func replaceNodeRole(nodeRole, in string) string {
	return strings.Replace(in, "{node-role}", nodeRole, -1)
}
