package cl_genesis
import (
	"fmt"
	"github.com/kurtosis-tech/eth2-merge-kurtosis-module/kurtosis-module/impl/service_launch_utils"
	"github.com/kurtosis-tech/kurtosis-core-api-lib/api/golang/lib/services"
	"github.com/kurtosis-tech/stacktrace"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"text/template"
	"time"
)

const (
	// The prefix that the directory for containing information about this CL genesis generation run will have
	//  inside the shared directory
	clGenesisGenerationInstanceSharedDirpathPrefix = "cl-genesis-"

	configDirname                      = "config"
	genesisGenerationConfigYmlFilename          = "config.yaml"
	genesisGenerationMnemonicsYmlFilename = "mnemonics.yaml"

	outputDirname = "output"
	tranchesDiranme = "tranches"
	genesisConfigYmlFilename = "config.yaml" // WARNING: Do not change this! The CL clients are hardcoded to look for this filename
	genesisStateFilename     = "genesis.ssz"
	deployBlockFilename      = "deploy_block.txt"
	depositContractFilename = "deposit_contract.txt"
	jwtSecretFilename = "jwtsecret"

	// Generation constants
	clGenesisGenerationBinaryFilepathOnContainer = "/usr/local/bin/eth2-testnet-genesis"
	deployBlock = "0"
	eth1Block              = "0x0000000000000000000000000000000000000000000000000000000000000000"
	successCommandExitCode = 0
)

type clGenesisConfigTemplateData struct {
	NetworkId                          string
	SecondsPerSlot                     uint32
	UnixTimestamp                      uint64
	TotalTerminalDifficulty            uint64
	AltairForkEpoch                    uint64
	MergeForkEpoch                     uint64
	NumValidatorKeysToPreregister uint32
	PreregisteredValidatorKeysMnemonic string
	DepositContractAddress string
}

func GenerateCLGenesisData(
	genesisGenerationConfigYmlTemplate *template.Template,
	genesisGenerationMnemonicsYmlTemplate *template.Template,
	jwtSecretFilepathOnModuleContainer string,
	serviceCtx *services.ServiceContext,
	genesisUnixTimestamp uint64,
	networkId string,
	depositContractAddress string,
	totalTerminalDifficulty uint64,
	secondsPerSlot uint32,
	altairForkEpoch uint64,
	mergeForkEpoch uint64,
	preregisteredValidatorKeysMnemonic string,
	numValidatorKeysToPreregister uint32,
) (
	*CLGenesisData,
	error,
) {
	sharedDir := serviceCtx.GetSharedDirectory()
	generationInstanceSharedDir := sharedDir.GetChildPath(fmt.Sprintf(
		"%v%v",
		clGenesisGenerationInstanceSharedDirpathPrefix,
		time.Now().Unix(),
	))
	configSharedDir := generationInstanceSharedDir.GetChildPath(configDirname)
	outputSharedDir := generationInstanceSharedDir.GetChildPath(outputDirname)

	allSharedDirsToCreate := []*services.SharedPath{
		generationInstanceSharedDir,
		configSharedDir,
		outputSharedDir,
	}
	for _, sharedDirToCreate := range allSharedDirsToCreate {
		toCreateDirpathOnModuleContainer := sharedDirToCreate.GetAbsPathOnThisContainer()
		if err := os.Mkdir(toCreateDirpathOnModuleContainer, os.ModePerm); err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred creating directory '%v'", toCreateDirpathOnModuleContainer)
		}
	}

	templateData := &clGenesisConfigTemplateData{
		NetworkId:                          networkId,
		SecondsPerSlot:                     secondsPerSlot,
		UnixTimestamp:                      genesisUnixTimestamp,
		TotalTerminalDifficulty:            totalTerminalDifficulty,
		AltairForkEpoch:                    altairForkEpoch,
		MergeForkEpoch:                     mergeForkEpoch,
		NumValidatorKeysToPreregister:      numValidatorKeysToPreregister,
		PreregisteredValidatorKeysMnemonic: preregisteredValidatorKeysMnemonic,
		DepositContractAddress:             depositContractAddress,
	}
	genesisGenerationConfigSharedFile, genesisGenerationMnemonicsSharedFile, err := createGenesisGenerationConfig(
		genesisGenerationConfigYmlTemplate,
		genesisGenerationMnemonicsYmlTemplate,
		templateData,
		configSharedDir,
	)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred creating the CL genesis generation config")
	}

	result, err := runClGenesisGeneration(
		genesisGenerationConfigSharedFile,
		genesisGenerationMnemonicsSharedFile,
		jwtSecretFilepathOnModuleContainer,
		genesisUnixTimestamp,
		depositContractAddress,
		serviceCtx,
		outputSharedDir,
	)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred running the CL genesis generation")
	}

	return result, nil
}

func createGenesisGenerationConfig(
	genesisGenerationConfigYmlTemplate *template.Template,
	genesisGenerationMnemonicsYmlTemplate *template.Template,
	templateData *clGenesisConfigTemplateData,
	configSharedDir *services.SharedPath,
) (
	resultConfigYmlSharedFile *services.SharedPath,
	resultMnemonicsYmlSharedFile *services.SharedPath,
	resultErr error,
){
	genesisGenerationConfigSharedFile := configSharedDir.GetChildPath(genesisGenerationConfigYmlFilename)
	if err := service_launch_utils.FillTemplateToSharedPath(genesisGenerationConfigYmlTemplate, templateData, genesisGenerationConfigSharedFile); err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred filling the CL genesis generation config YML template")
	}

	genesisGenerationMnemonicsSharedFile := configSharedDir.GetChildPath(genesisGenerationMnemonicsYmlFilename)
	if err := service_launch_utils.FillTemplateToSharedPath(genesisGenerationMnemonicsYmlTemplate, templateData, genesisGenerationMnemonicsSharedFile); err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred filling the CL genesis generation mnemonics YML template")
	}

	return genesisGenerationConfigSharedFile, genesisGenerationMnemonicsSharedFile, nil
}

func runClGenesisGeneration(
	genesisGenerationConfigSharedFile *services.SharedPath,
	genesisGenerationMnemonicsSharedFile *services.SharedPath,
	jwtSecretFilepathOnModuleContainer string,
	genesisTimestamp uint64,
	depositContractAddress string,
	serviceCtx *services.ServiceContext,
	outputSharedDir *services.SharedPath,
) (
	*CLGenesisData,
	error,
){
	// Copy the genesis config file to output directory
	genesisGenerationConfigFilepathOnThisContainer := genesisGenerationConfigSharedFile.GetAbsPathOnThisContainer()
	genesisGenerationConfigSrcFp, err := os.Open(genesisGenerationConfigFilepathOnThisContainer)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred opening genesis generation config file '%v' for reading", genesisGenerationConfigFilepathOnThisContainer)
	}
	genesisConfigSharedFile := outputSharedDir.GetChildPath(genesisConfigYmlFilename)
	genesisConfigFilepathOnThisContainer := genesisConfigSharedFile.GetAbsPathOnThisContainer()
	genesisConfigFp, err := os.Create(genesisConfigFilepathOnThisContainer)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred opening genesis config file '%v' for writing", genesisConfigFilepathOnThisContainer)
	}
	if _, err := io.Copy(genesisConfigFp, genesisGenerationConfigSrcFp); err != nil {
		return nil, stacktrace.Propagate(
			err,
			"An error occurred copying the genesis generation config file '%v' to '%v'",
			genesisGenerationConfigFilepathOnThisContainer,
			genesisConfigFilepathOnThisContainer,
		 )
	}

	// Create deploy block file
	deployBlockSharedFile := outputSharedDir.GetChildPath(deployBlockFilename)
	deployBlockFilepathOnThisContainer := deployBlockSharedFile.GetAbsPathOnThisContainer()
	if err := ioutil.WriteFile(deployBlockFilepathOnThisContainer, []byte(deployBlock), os.ModePerm); err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred writing the deploy block file at '%v'", deployBlockFilepathOnThisContainer)
	}

	// Create deposit contract file
	depositContractSharedFile := outputSharedDir.GetChildPath(depositContractFilename)
	depositContractFilepathOnThisContainer := depositContractSharedFile.GetAbsPathOnThisContainer()
	if err := ioutil.WriteFile(depositContractFilepathOnThisContainer, []byte(depositContractAddress), os.ModePerm); err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred writing the deposit contract file at '%v'", depositContractFilepathOnThisContainer)
	}

	genesisStateSharedFile := outputSharedDir.GetChildPath(genesisStateFilename)
	tranchesSharedDir := outputSharedDir.GetChildPath(tranchesDiranme)

	clGenesisGenerationCmdArgs := []string{
		clGenesisGenerationBinaryFilepathOnContainer,
		"phase0",
		"--config", genesisGenerationConfigSharedFile.GetAbsPathOnServiceContainer(),
		"--eth1-block", eth1Block,
		"--mnemonics", genesisGenerationMnemonicsSharedFile.GetAbsPathOnServiceContainer(),
		"--timestamp", fmt.Sprintf("%v", genesisTimestamp),
		"--tranches-dir", tranchesSharedDir.GetAbsPathOnServiceContainer(),
		"--state-output", genesisStateSharedFile.GetAbsPathOnServiceContainer(),
	}

	genesisGenerationExitCode, genesisGenerationOutput, err := serviceCtx.ExecCommand(clGenesisGenerationCmdArgs)
	if err != nil {
		return nil, stacktrace.Propagate(
			err,
			"An error occurred executing command '%v' to generate the CL genesis data",
			strings.Join(clGenesisGenerationCmdArgs, " "),
		 )
	}
	if genesisGenerationExitCode != successCommandExitCode {
		return nil, stacktrace.NewError(
			"Expected CL genesis data generation command '%v' to return exit code '%v' but returned '%v' with the following logs:\n%v",
			strings.Join(clGenesisGenerationCmdArgs, " "),
			successCommandExitCode,
			genesisGenerationExitCode,
			genesisGenerationOutput,
		 )
	}

	jwtSecretSharedFile := outputSharedDir.GetChildPath(jwtSecretFilename)
	if err := service_launch_utils.CopyFileToSharedPath(jwtSecretFilepathOnModuleContainer, jwtSecretSharedFile); err != nil {
		return nil, stacktrace.Propagate(
			err,
			"An error occurred copying JWT secret file from path '%v' to shared filepath '%v'",
			jwtSecretFilepathOnModuleContainer,
			jwtSecretSharedFile.GetAbsPathOnThisContainer(),
		)
	}

	result := newCLGenesisData(
		outputSharedDir.GetAbsPathOnThisContainer(),
		jwtSecretSharedFile.GetAbsPathOnThisContainer(),
		genesisConfigSharedFile.GetAbsPathOnThisContainer(),
		genesisStateSharedFile.GetAbsPathOnThisContainer(),
	)
	return result, nil
}