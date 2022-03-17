package prysm

import (
	"fmt"
	"github.com/kurtosis-tech/eth2-merge-kurtosis-module/kurtosis-module/impl/module_io"
	"github.com/kurtosis-tech/eth2-merge-kurtosis-module/kurtosis-module/impl/participant_network/cl"
	"github.com/kurtosis-tech/eth2-merge-kurtosis-module/kurtosis-module/impl/participant_network/cl/cl_client_rest_client"
	"github.com/kurtosis-tech/eth2-merge-kurtosis-module/kurtosis-module/impl/participant_network/el"
	cl2 "github.com/kurtosis-tech/eth2-merge-kurtosis-module/kurtosis-module/impl/prelaunch_data_generator/cl_validator_keystores"
	"github.com/kurtosis-tech/eth2-merge-kurtosis-module/kurtosis-module/impl/service_launch_utils"
	"github.com/kurtosis-tech/kurtosis-core-api-lib/api/golang/lib/enclaves"
	"github.com/kurtosis-tech/kurtosis-core-api-lib/api/golang/lib/services"
	"github.com/kurtosis-tech/stacktrace"
	recursive_copy "github.com/otiai10/copy"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"
)

const (
	imageSeparatorDelimiter = ","
	expectedNumImages       = 2

	consensusDataDirpathOnServiceContainer = "/consensus-data"

	// Port IDs
	tcpDiscoveryPortID        = "tcpDiscovery"
	udpDiscoveryPortID        = "udpDiscovery"
	rpcPortID                 = "rpc"
	httpPortID                = "http"
	beaconMonitoringPortID    = "monitoring"
	validatorMonitoringPortID = "monitoring"

	// Port nums
	discoveryTCPPortNum        uint16 = 13000
	discoveryUDPPortNum        uint16 = 12000
	rpcPortNum                 uint16 = 4000
	httpPortNum                uint16 = 3500
	beaconMonitoringPortNum    uint16 = 8080
	validatorMonitoringPortNum uint16 = 8081

	genesisConfigYmlRelFilepathInSharedDir = "genesis-config.yml"
	genesisSszRelFilepathInSharedDir       = "genesis.ssz"
	prysmPasswordTxtRelFilepathInSharedDir = "prysm-password.txt"

	validatorKeysRelDirpathInSharedDir    = "validator-keys"
	validatorSecretsRelDirpathInSharedDir = "validator-secrets"

	maxNumHealthcheckRetries      = 100
	timeBetweenHealthcheckRetries = 5 * time.Second

	beaconSuffixServiceId    = "beacon"
	validatorSuffixServiceId = "validator"

	minPeers = 1

	metricsPath = "/metrics"
)

var beaconNodeUsedPorts = map[string]*services.PortSpec{
	tcpDiscoveryPortID:     services.NewPortSpec(discoveryTCPPortNum, services.PortProtocol_TCP),
	udpDiscoveryPortID:     services.NewPortSpec(discoveryUDPPortNum, services.PortProtocol_UDP),
	rpcPortID:              services.NewPortSpec(rpcPortNum, services.PortProtocol_TCP),
	httpPortID:             services.NewPortSpec(httpPortNum, services.PortProtocol_TCP),
	beaconMonitoringPortID: services.NewPortSpec(beaconMonitoringPortNum, services.PortProtocol_TCP),
}

var validatorNodeUsedPorts = map[string]*services.PortSpec{
	validatorMonitoringPortID: services.NewPortSpec(validatorMonitoringPortNum, services.PortProtocol_TCP),
}
var prysmLogLevels = map[module_io.GlobalClientLogLevel]string{
	module_io.GlobalClientLogLevel_Error: "error",
	module_io.GlobalClientLogLevel_Warn:  "warn",
	module_io.GlobalClientLogLevel_Info:  "info",
	module_io.GlobalClientLogLevel_Debug: "debug",
	module_io.GlobalClientLogLevel_Trace: "trace",
}

type PrysmCLClientLauncher struct {
	genesisConfigYmlFilepathOnModuleContainer string
	genesisSszFilepathOnModuleContainer       string
	jwtSecretFilepathOnModuleContainer        string
	prysmPassword                             string
}

func NewPrysmCLClientLauncher(genesisConfigYmlFilepathOnModuleContainer string, genesisSszFilepathOnModuleContainer string, jwtSecretFilepathOnModuleContainer string, prysmPassword string) *PrysmCLClientLauncher {
	return &PrysmCLClientLauncher{genesisConfigYmlFilepathOnModuleContainer: genesisConfigYmlFilepathOnModuleContainer, genesisSszFilepathOnModuleContainer: genesisSszFilepathOnModuleContainer, jwtSecretFilepathOnModuleContainer: jwtSecretFilepathOnModuleContainer, prysmPassword: prysmPassword}
}

func (launcher *PrysmCLClientLauncher) Launch(
	enclaveCtx *enclaves.EnclaveContext,
	serviceId services.ServiceID,
	// NOTE: Because Prysm has separate images for Beacon and validator, this string will actually be a delimited
	//  combination of both Beacon & validator images
	delimitedImagesStr string,
	participantLogLevel string,
	globalLogLevel module_io.GlobalClientLogLevel,
	bootnodeContext *cl.CLClientContext,
	elClientContext *el.ELClientContext,
	nodeKeystoreDirpaths *cl2.NodeTypeKeystoreDirpaths,
	extraBeaconParams []string,
	extraValidatorParams []string,
) (resultClientCtx *cl.CLClientContext, resultErr error) {
	imageStrs := strings.Split(delimitedImagesStr, imageSeparatorDelimiter)
	if len(imageStrs) != expectedNumImages {
		return nil, stacktrace.NewError(
			"Expected Prysm image string '%v' to contain %v images - Beacon and validator - delimited by '%v'",
			delimitedImagesStr,
			expectedNumImages,
			imageSeparatorDelimiter,
		)
	}
	beaconImage := imageStrs[0]
	validatorImage := imageStrs[1]
	if len(strings.TrimSpace(beaconImage)) == 0 {
		return nil, stacktrace.NewError("An empty Prysm Beacon image was provided")
	}
	if len(strings.TrimSpace(validatorImage)) == 0 {
		return nil, stacktrace.NewError("An empty Prysm validator image was provided")
	}

	beaconNodeServiceId := services.ServiceID(fmt.Sprintf("%v-%v", serviceId, beaconSuffixServiceId))
	validatorNodeServiceId := services.ServiceID(fmt.Sprintf("%v-%v", serviceId, validatorSuffixServiceId))

	logLevel, err := module_io.GetClientLogLevelStrOrDefault(participantLogLevel, globalLogLevel, prysmLogLevels)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting the client log level using participant log level '%v' and global log level '%v'", participantLogLevel, globalLogLevel)
	}

	beaconContainerConfigSupplier := launcher.getBeaconContainerConfigSupplier(
		beaconImage,
		bootnodeContext,
		elClientContext,
		logLevel,
		launcher.genesisConfigYmlFilepathOnModuleContainer,
		launcher.genesisSszFilepathOnModuleContainer,
		extraBeaconParams,
	)
	beaconServiceCtx, err := enclaveCtx.AddService(beaconNodeServiceId, beaconContainerConfigSupplier)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred launching the Prysm CL beacon client with service ID '%v'", serviceId)
	}

	httpPort, found := beaconServiceCtx.GetPrivatePorts()[httpPortID]
	if !found {
		return nil, stacktrace.NewError("Expected new Prysm beacon service to have port with ID '%v', but none was found", httpPortID)
	}

	beaconRestClient := cl_client_rest_client.NewCLClientRESTClient(beaconServiceCtx.GetPrivateIPAddress(), httpPort.GetNumber())
	if err := cl.WaitForBeaconClientAvailability(beaconRestClient, maxNumHealthcheckRetries, timeBetweenHealthcheckRetries); err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred waiting for the new Prysm beacon node to become available")
	}

	nodeIdentity, err := beaconRestClient.GetNodeIdentity()
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting the new Prysm beacon node's identity, which is necessary to retrieve its ENR")
	}

	beaconRPCEndpoint := fmt.Sprintf("%v:%v", beaconServiceCtx.GetPrivateIPAddress(), rpcPortNum)
	beaconHTTPEndpoint := fmt.Sprintf("%v:%v", beaconServiceCtx.GetPrivateIPAddress(), httpPortNum)
	validatorContainerConfigSupplier := launcher.getValidatorContainerConfigSupplier(
		validatorImage,
		validatorNodeServiceId,
		logLevel,
		beaconRPCEndpoint,
		beaconHTTPEndpoint,
		nodeKeystoreDirpaths.RawKeysDirpath,
		nodeKeystoreDirpaths.PrysmDirpath,
		extraValidatorParams,
	)
	validatorServiceCtx, err := enclaveCtx.AddService(validatorNodeServiceId, validatorContainerConfigSupplier)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred launching the Prysm CL validator client with service ID '%v'", serviceId)
	}

	beaconMonitoringPort, found := beaconServiceCtx.GetPrivatePorts()[beaconMonitoringPortID]
	if !found {
		return nil, stacktrace.NewError("Expected new Prysm Beacon service to have port with ID '%v', but none was found", beaconMonitoringPortID)
	}
	beaconMetricsUrl := fmt.Sprintf("%v:%v", beaconServiceCtx.GetPrivateIPAddress(), beaconMonitoringPort.GetNumber())

	validatorMonitoringPort, found := validatorServiceCtx.GetPrivatePorts()[validatorMonitoringPortID]
	if !found {
		return nil, stacktrace.NewError("Expected new Prysm Validator service to have port with ID '%v', but none was found", validatorMonitoringPortID)
	}
	validatorMetricsUrl := fmt.Sprintf("%v:%v", validatorServiceCtx.GetPrivateIPAddress(), validatorMonitoringPort.GetNumber())

	beaconNodeMetricsInfo := cl.NewCLNodeMetricsInfo(string(beaconNodeServiceId), metricsPath, beaconMetricsUrl)
	validatorNodeMetricsInfo := cl.NewCLNodeMetricsInfo(string(validatorNodeServiceId), metricsPath, validatorMetricsUrl)
	nodesMetricsInfo := []*cl.CLNodeMetricsInfo{beaconNodeMetricsInfo, validatorNodeMetricsInfo}

	result := cl.NewCLClientContext(
		nodeIdentity.ENR,
		beaconServiceCtx.GetPrivateIPAddress(),
		httpPortNum,
		nodesMetricsInfo,
		beaconRestClient,
	)

	return result, nil

}

// ====================================================================================================
//                                   Private Helper Methods
// ====================================================================================================
func (launcher *PrysmCLClientLauncher) getBeaconContainerConfigSupplier(
	beaconImage string,
	bootnodeContext *cl.CLClientContext, // If this is empty, the node will be launched as a bootnode
	elClientContext *el.ELClientContext,
	logLevel string,
	genesisConfigYmlFilepathOnModuleContainer string,
	genesisSszFilepathOnModuleContainer string,
	extraParams []string,
) func(string, *services.SharedPath) (*services.ContainerConfig, error) {
	containerConfigSupplier := func(privateIpAddr string, sharedDir *services.SharedPath) (*services.ContainerConfig, error) {

		genesisConfigYmlSharedPath := sharedDir.GetChildPath(genesisConfigYmlRelFilepathInSharedDir)
		if err := service_launch_utils.CopyFileToSharedPath(genesisConfigYmlFilepathOnModuleContainer, genesisConfigYmlSharedPath); err != nil {
			return nil, stacktrace.Propagate(
				err,
				"An error occurred copying the genesis config YML from '%v' to shared dir relative path '%v'",
				genesisConfigYmlFilepathOnModuleContainer,
				genesisConfigYmlRelFilepathInSharedDir,
			)
		}

		genesisSszSharedPath := sharedDir.GetChildPath(genesisSszRelFilepathInSharedDir)
		if err := service_launch_utils.CopyFileToSharedPath(genesisSszFilepathOnModuleContainer, genesisSszSharedPath); err != nil {
			return nil, stacktrace.Propagate(
				err,
				"An error occurred copying the genesis SSZ from '%v' to shared dir relative path '%v'",
				genesisSszFilepathOnModuleContainer,
				genesisSszRelFilepathInSharedDir,
			)
		}

		elClientRpcUrlStr := fmt.Sprintf(
			"http://%v:%v",
			elClientContext.GetIPAddress(),
			elClientContext.GetRPCPortNum(),
		)

		cmdArgs := []string{
			"--accept-terms-of-use=true", //it's mandatory in order to run the node
			"--prater",                   //it's a tesnet setup, it's mandatory to set a network (https://docs.prylabs.network/docs/install/install-with-script#before-you-begin-pick-your-network-1)
			"--datadir=" + consensusDataDirpathOnServiceContainer,
			"--chain-config-file=" + genesisConfigYmlSharedPath.GetAbsPathOnServiceContainer(),
			"--genesis-state=" + genesisSszSharedPath.GetAbsPathOnServiceContainer(),
			"--http-web3provider=" + elClientRpcUrlStr,
			"--execution-provider=" + elClientRpcUrlStr,
			"--http-modules=prysm,eth",
			"--rpc-host=" + privateIpAddr,
			fmt.Sprintf("--rpc-port=%v", rpcPortNum),
			"--grpc-gateway-host=0.0.0.0",
			fmt.Sprintf("--grpc-gateway-port=%v", httpPortNum),
			fmt.Sprintf("--p2p-tcp-port=%v", discoveryTCPPortNum),
			fmt.Sprintf("--p2p-udp-port=%v", discoveryUDPPortNum),
			fmt.Sprintf("--min-sync-peers=%v", minPeers),
			"--monitoring-host=" + privateIpAddr,
			fmt.Sprintf("--monitoring-port=%v", beaconMonitoringPortNum),
			"--verbosity=" + logLevel,
			// Set per Pari's recommendation to reduce noise
			"--subscribe-all-subnets=true",
			// TODO SOMETHING ABOUT JWT SECRET?
			// vvvvvvvvvvvvvvvvvvv METRICS CONFIG vvvvvvvvvvvvvvvvvvvvv
			"--disable-monitoring=false",
			"--monitoring-host=" + privateIpAddr,
			fmt.Sprintf("--monitoring-port=%v", beaconMonitoringPortNum),
			// ^^^^^^^^^^^^^^^^^^^ METRICS CONFIG ^^^^^^^^^^^^^^^^^^^^^
		}
		if bootnodeContext != nil {
			cmdArgs = append(cmdArgs, "--bootstrap-node="+bootnodeContext.GetENR())
		}
		if len(extraParams) > 0 {
			cmdArgs = append(cmdArgs, extraParams...)
		}

		containerConfig := services.NewContainerConfigBuilder(
			beaconImage,
		).WithUsedPorts(
			beaconNodeUsedPorts,
		).WithCmdOverride(
			cmdArgs,
		).Build()

		return containerConfig, nil
	}
	return containerConfigSupplier
}

func (launcher *PrysmCLClientLauncher) getValidatorContainerConfigSupplier(
	validatorImage string,
	serviceId services.ServiceID,
	logLevel string,
	beaconRPCEndpoint string,
	beaconHTTPEndpoint string,
	validatorKeysDirpathOnModuleContainer string,
	validatorSecretsDirpathOnModuleContainer string,
	extraParams []string,
) func(string, *services.SharedPath) (*services.ContainerConfig, error) {
	containerConfigSupplier := func(privateIpAddr string, sharedDir *services.SharedPath) (*services.ContainerConfig, error) {

		genesisConfigYmlSharedPath := sharedDir.GetChildPath(genesisConfigYmlRelFilepathInSharedDir)
		if err := service_launch_utils.CopyFileToSharedPath(launcher.genesisConfigYmlFilepathOnModuleContainer, genesisConfigYmlSharedPath); err != nil {
			return nil, stacktrace.Propagate(
				err,
				"An error occurred copying the genesis config YML from '%v' to shared dir relative path '%v'",
				launcher.genesisConfigYmlFilepathOnModuleContainer,
				genesisConfigYmlRelFilepathInSharedDir,
			)
		}

		validatorKeysSharedPath := sharedDir.GetChildPath(validatorKeysRelDirpathInSharedDir)
		if err := recursive_copy.Copy(
			validatorKeysDirpathOnModuleContainer,
			validatorKeysSharedPath.GetAbsPathOnThisContainer(),
		); err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred copying the validator keys into the shared directory so the node can consume them")
		}

		validatorSecretsSharedPath := sharedDir.GetChildPath(validatorSecretsRelDirpathInSharedDir)
		if err := recursive_copy.Copy(
			validatorSecretsDirpathOnModuleContainer,
			validatorSecretsSharedPath.GetAbsPathOnThisContainer(),
		); err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred copying the validator secrets into the shared directory so the node can consume them")
		}

		prysmPasswordTxtSharedPath := sharedDir.GetChildPath(prysmPasswordTxtRelFilepathInSharedDir)
		prysmPasswordTxtFilepathOnModuleContainer := prysmPasswordTxtSharedPath.GetAbsPathOnThisContainer()
		if err := ioutil.WriteFile(prysmPasswordTxtFilepathOnModuleContainer, []byte(launcher.prysmPassword), os.ModePerm); err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred writing the Prysm keystore password to file '%v'", prysmPasswordTxtFilepathOnModuleContainer)
		}

		rootDirpath := path.Join(consensusDataDirpathOnServiceContainer, string(serviceId))

		cmdArgs := []string{
			"--accept-terms-of-use=true", //it's mandatory in order to run the node
			"--prater",                   //it's a tesnet setup, it's mandatory to set a network (https://docs.prylabs.network/docs/install/install-with-script#before-you-begin-pick-your-network-1)
			"--beacon-rpc-gateway-provider=" + beaconHTTPEndpoint,
			"--beacon-rpc-provider=" + beaconRPCEndpoint,
			"--wallet-dir=" + validatorSecretsSharedPath.GetAbsPathOnServiceContainer(),
			"--wallet-password-file=" + prysmPasswordTxtSharedPath.GetAbsPathOnServiceContainer(),
			"--datadir=" + rootDirpath,
			"--monitoring-host=" + privateIpAddr,
			fmt.Sprintf("--monitoring-port=%v", validatorMonitoringPortNum),
			"--verbosity=" + logLevel,
			// TODO SOMETHING ABOUT JWT
			// vvvvvvvvvvvvvvvvvvv METRICS CONFIG vvvvvvvvvvvvvvvvvvvvv
			"--disable-monitoring=false",
			"--monitoring-host=" + privateIpAddr,
			fmt.Sprintf("--monitoring-port=%v", validatorMonitoringPortNum),
			// ^^^^^^^^^^^^^^^^^^^ METRICS CONFIG ^^^^^^^^^^^^^^^^^^^^^
		}
		if len(extraParams) > 0 {
			cmdArgs = append(cmdArgs, extraParams...)
		}

		containerConfig := services.NewContainerConfigBuilder(
			validatorImage,
		).WithUsedPorts(
			validatorNodeUsedPorts,
		).WithCmdOverride(
			cmdArgs,
		).Build()

		return containerConfig, nil
	}
	return containerConfigSupplier
}
