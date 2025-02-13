package impl

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"

	. "github.com/alcionai/corso/src/cli/print"
	"github.com/alcionai/corso/src/internal/common"
	"github.com/alcionai/corso/src/internal/connector"
	"github.com/alcionai/corso/src/internal/connector/graph"
	"github.com/alcionai/corso/src/internal/connector/mockconnector"
	"github.com/alcionai/corso/src/internal/data"
	"github.com/alcionai/corso/src/internal/version"
	"github.com/alcionai/corso/src/pkg/account"
	"github.com/alcionai/corso/src/pkg/backup/details"
	"github.com/alcionai/corso/src/pkg/control"
	"github.com/alcionai/corso/src/pkg/credentials"
	"github.com/alcionai/corso/src/pkg/fault"
	"github.com/alcionai/corso/src/pkg/path"
	"github.com/alcionai/corso/src/pkg/selectors"
)

var (
	Count       int
	Destination string
	Tenant      string
	User        string
)

// TODO: ErrGenerating       = errors.New("not all items were successfully generated")

var ErrNotYetImplemeted = errors.New("not yet implemented")

// ------------------------------------------------------------------------------------------
// Restoration
// ------------------------------------------------------------------------------------------

type dataBuilderFunc func(id, now, subject, body string) []byte

func generateAndRestoreItems(
	ctx context.Context,
	gc *connector.GraphConnector,
	acct account.Account,
	service path.ServiceType,
	cat path.CategoryType,
	sel selectors.Selector,
	tenantID, userID, destFldr string,
	howMany int,
	dbf dataBuilderFunc,
	opts control.Options,
	errs *fault.Errors,
) (*details.Details, error) {
	items := make([]item, 0, howMany)

	for i := 0; i < howMany; i++ {
		var (
			now       = common.Now()
			nowLegacy = common.FormatLegacyTime(time.Now())
			id        = uuid.NewString()
			subject   = "automated " + now[:16] + " - " + id[:8]
			body      = "automated " + cat.String() + " generation for " + userID + " at " + now + " - " + id
		)

		items = append(items, item{
			name: id,
			data: dbf(id, nowLegacy, subject, body),
		})
	}

	collections := []collection{{
		pathElements: []string{destFldr},
		category:     cat,
		items:        items,
	}}

	// TODO: fit the destination to the containers
	dest := control.DefaultRestoreDestination(common.SimpleTimeTesting)
	dest.ContainerName = destFldr

	dataColls, err := buildCollections(
		service,
		tenantID, userID,
		dest,
		collections,
	)
	if err != nil {
		return nil, err
	}

	Infof(ctx, "Generating %d %s items in %s\n", howMany, cat, Destination)

	return gc.RestoreDataCollections(ctx, version.Backup, acct, sel, dest, opts, dataColls, errs)
}

// ------------------------------------------------------------------------------------------
// Common Helpers
// ------------------------------------------------------------------------------------------

func getGCAndVerifyUser(ctx context.Context, userID string) (*connector.GraphConnector, account.Account, error) {
	tid := common.First(Tenant, os.Getenv(account.AzureTenantID))

	// get account info
	m365Cfg := account.M365Config{
		M365:          credentials.GetM365(),
		AzureTenantID: tid,
	}

	acct, err := account.NewAccount(account.ProviderM365, m365Cfg)
	if err != nil {
		return nil, account.Account{}, errors.Wrap(err, "finding m365 account details")
	}

	// build a graph connector
	// TODO: log/print recoverable errors
	errs := fault.New(false)

	gc, err := connector.NewGraphConnector(ctx, graph.HTTPClient(graph.NoTimeout()), acct, connector.Users, errs)
	if err != nil {
		return nil, account.Account{}, errors.Wrap(err, "connecting to graph api")
	}

	normUsers := map[string]struct{}{}

	for k := range gc.Users {
		normUsers[strings.ToLower(k)] = struct{}{}
	}

	if _, ok := normUsers[strings.ToLower(User)]; !ok {
		return nil, account.Account{}, errors.New("user not found within tenant")
	}

	return gc, acct, nil
}

type item struct {
	name string
	data []byte
}

type collection struct {
	// Elements (in order) for the path representing this collection. Should
	// only contain elements after the prefix that corso uses for the path. For
	// example, a collection for the Inbox folder in exchange mail would just be
	// "Inbox".
	pathElements []string
	category     path.CategoryType
	items        []item
}

func buildCollections(
	service path.ServiceType,
	tenant, user string,
	dest control.RestoreDestination,
	colls []collection,
) ([]data.RestoreCollection, error) {
	collections := make([]data.RestoreCollection, 0, len(colls))

	for _, c := range colls {
		pth, err := toDataLayerPath(
			service,
			tenant,
			user,
			c.category,
			c.pathElements,
			false,
		)
		if err != nil {
			return nil, err
		}

		mc := mockconnector.NewMockExchangeCollection(pth, pth, len(c.items))

		for i := 0; i < len(c.items); i++ {
			mc.Names[i] = c.items[i].name
			mc.Data[i] = c.items[i].data
		}

		collections = append(collections, data.NotFoundRestoreCollection{Collection: mc})
	}

	return collections, nil
}

func toDataLayerPath(
	service path.ServiceType,
	tenant, user string,
	category path.CategoryType,
	elements []string,
	isItem bool,
) (path.Path, error) {
	pb := path.Builder{}.Append(elements...)

	switch service {
	case path.ExchangeService:
		return pb.ToDataLayerExchangePathForCategory(tenant, user, category, isItem)
	case path.OneDriveService:
		return pb.ToDataLayerOneDrivePath(tenant, user, isItem)
	}

	return nil, errors.Errorf("unknown service %s", service.String())
}
