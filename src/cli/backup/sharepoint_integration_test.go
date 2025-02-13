package backup_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/alcionai/corso/src/cli"
	"github.com/alcionai/corso/src/cli/config"
	"github.com/alcionai/corso/src/cli/print"
	"github.com/alcionai/corso/src/cli/utils"
	"github.com/alcionai/corso/src/internal/operations"
	"github.com/alcionai/corso/src/internal/tester"
	"github.com/alcionai/corso/src/pkg/account"
	"github.com/alcionai/corso/src/pkg/control"
	"github.com/alcionai/corso/src/pkg/repository"
	"github.com/alcionai/corso/src/pkg/selectors"
	"github.com/alcionai/corso/src/pkg/storage"
)

// ---------------------------------------------------------------------------
// tests with no prior backup
// ---------------------------------------------------------------------------

type NoBackupSharePointIntegrationSuite struct {
	tester.Suite
	acct       account.Account
	st         storage.Storage
	vpr        *viper.Viper
	cfgFP      string
	repo       repository.Repository
	m365SiteID string
	recorder   strings.Builder
}

func TestNoBackupSharePointIntegrationSuite(t *testing.T) {
	suite.Run(t, &NoBackupSharePointIntegrationSuite{Suite: tester.NewIntegrationSuite(
		t,
		[][]string{tester.AWSStorageCredEnvs, tester.M365AcctCredEnvs},
		tester.CorsoCLITests, tester.CorsoCLIBackupTests)})
}

func (suite *NoBackupSharePointIntegrationSuite) SetupSuite() {
	t := suite.T()
	ctx, flush := tester.NewContext()

	defer flush()

	// prepare common details
	suite.acct = tester.NewM365Account(t)
	suite.st = tester.NewPrefixedS3Storage(t)

	cfg, err := suite.st.S3Config()
	require.NoError(t, err)

	force := map[string]string{
		tester.TestCfgAccountProvider: "M365",
		tester.TestCfgStorageProvider: "S3",
		tester.TestCfgPrefix:          cfg.Prefix,
	}

	suite.vpr, suite.cfgFP = tester.MakeTempTestConfigClone(t, force)

	ctx = config.SetViper(ctx, suite.vpr)
	suite.m365SiteID = tester.M365SiteID(t)

	// init the repo first
	suite.repo, err = repository.Initialize(ctx, suite.acct, suite.st, control.Options{})
	require.NoError(t, err)
}

func (suite *NoBackupSharePointIntegrationSuite) TestSharePointBackupListCmd_empty() {
	t := suite.T()
	ctx, flush := tester.NewContext()
	ctx = config.SetViper(ctx, suite.vpr)

	defer flush()

	suite.recorder.Reset()

	cmd := tester.StubRootCmd(
		"backup", "list", "sharepoint",
		"--config-file", suite.cfgFP)
	cli.BuildCommandTree(cmd)

	cmd.SetErr(&suite.recorder)

	ctx = print.SetRootCmd(ctx, cmd)

	// run the command
	require.NoError(t, cmd.ExecuteContext(ctx))

	result := suite.recorder.String()

	// as an offhand check: the result should contain the m365 sitet id
	assert.Equal(t, "No backups available\n", result)
}

// ---------------------------------------------------------------------------
// tests for deleting backups
// ---------------------------------------------------------------------------

type BackupDeleteSharePointIntegrationSuite struct {
	tester.Suite
	acct     account.Account
	st       storage.Storage
	vpr      *viper.Viper
	cfgFP    string
	repo     repository.Repository
	backupOp operations.BackupOperation
	recorder strings.Builder
}

func TestBackupDeleteSharePointIntegrationSuite(t *testing.T) {
	suite.Run(t, &BackupDeleteSharePointIntegrationSuite{
		Suite: tester.NewIntegrationSuite(
			t,
			[][]string{tester.AWSStorageCredEnvs, tester.M365AcctCredEnvs},
			tester.CorsoCLITests, tester.CorsoCLIBackupTests),
	})
}

func (suite *BackupDeleteSharePointIntegrationSuite) SetupSuite() {
	t := suite.T()

	// prepare common details
	suite.acct = tester.NewM365Account(t)
	suite.st = tester.NewPrefixedS3Storage(t)

	cfg, err := suite.st.S3Config()
	require.NoError(t, err)

	force := map[string]string{
		tester.TestCfgAccountProvider: "M365",
		tester.TestCfgStorageProvider: "S3",
		tester.TestCfgPrefix:          cfg.Prefix,
	}
	suite.vpr, suite.cfgFP = tester.MakeTempTestConfigClone(t, force)

	ctx, flush := tester.NewContext()
	ctx = config.SetViper(ctx, suite.vpr)

	defer flush()

	// init the repo first
	suite.repo, err = repository.Initialize(ctx, suite.acct, suite.st, control.Options{})
	require.NoError(t, err)

	m365SiteID := tester.M365SiteID(t)
	sites := []string{m365SiteID}

	// some tests require an existing backup
	sel := selectors.NewSharePointBackup(sites)
	sel.Include(sel.Libraries(selectors.Any()))

	suite.backupOp, err = suite.repo.NewBackup(ctx, sel.Selector)
	require.NoError(t, suite.backupOp.Run(ctx))
	require.NoError(t, err)
}

func (suite *BackupDeleteSharePointIntegrationSuite) TestSharePointBackupDeleteCmd() {
	t := suite.T()
	ctx, flush := tester.NewContext()
	ctx = config.SetViper(ctx, suite.vpr)

	defer flush()

	suite.recorder.Reset()

	cmd := tester.StubRootCmd(
		"backup", "delete", "sharepoint",
		"--config-file", suite.cfgFP,
		"--"+utils.BackupFN, string(suite.backupOp.Results.BackupID))
	cli.BuildCommandTree(cmd)
	cmd.SetErr(&suite.recorder)

	ctx = print.SetRootCmd(ctx, cmd)

	// run the command
	require.NoError(t, cmd.ExecuteContext(ctx))

	result := suite.recorder.String()

	assert.Equal(t, fmt.Sprintf("Deleted SharePoint backup %s\n", string(suite.backupOp.Results.BackupID)), result)
}

// moved out of the func above to make the linter happy
// // a follow-up details call should fail, due to the backup ID being deleted
// cmd = tester.StubRootCmd(
// 	"backup", "details", "sharepoint",
// 	"--config-file", suite.cfgFP,
// 	"--backup", string(suite.backupOp.Results.BackupID))
// cli.BuildCommandTree(cmd)

// require.Error(t, cmd.ExecuteContext(ctx))

func (suite *BackupDeleteSharePointIntegrationSuite) TestSharePointBackupDeleteCmd_unknownID() {
	t := suite.T()
	ctx, flush := tester.NewContext()
	ctx = config.SetViper(ctx, suite.vpr)

	defer flush()

	cmd := tester.StubRootCmd(
		"backup", "delete", "sharepoint",
		"--config-file", suite.cfgFP,
		"--"+utils.BackupFN, uuid.NewString())
	cli.BuildCommandTree(cmd)

	// unknown backupIDs should error since the modelStore can't find the backup
	require.Error(t, cmd.ExecuteContext(ctx))
}
