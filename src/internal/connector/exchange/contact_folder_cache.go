package exchange

import (
	"context"

	"github.com/alcionai/clues"
	"github.com/pkg/errors"

	"github.com/alcionai/corso/src/internal/connector/graph"
	"github.com/alcionai/corso/src/pkg/fault"
	"github.com/alcionai/corso/src/pkg/path"
)

var _ graph.ContainerResolver = &contactFolderCache{}

type contactFolderCache struct {
	*containerResolver
	enumer containersEnumerator
	getter containerGetter
	userID string
}

func (cfc *contactFolderCache) populateContactRoot(
	ctx context.Context,
	directoryID string,
	baseContainerPath []string,
) error {
	f, err := cfc.getter.GetContainerByID(ctx, cfc.userID, directoryID)
	if err != nil {
		return clues.Wrap(err, "fetching root folder")
	}

	temp := graph.NewCacheFolder(
		f,
		path.Builder{}.Append(baseContainerPath...), // storage path
		path.Builder{}.Append(baseContainerPath...)) // display location
	if err := cfc.addFolder(temp); err != nil {
		return clues.Wrap(err, "adding resolver dir").WithClues(ctx)
	}

	return nil
}

// Populate is utility function for placing cache container
// objects into the Contact Folder Cache
// Function does NOT use Delta Queries as it is not supported
// as of (Oct-07-2022)
func (cfc *contactFolderCache) Populate(
	ctx context.Context,
	errs *fault.Errors,
	baseID string,
	baseContainerPather ...string,
) error {
	if err := cfc.init(ctx, baseID, baseContainerPather); err != nil {
		return errors.Wrap(err, "initializing")
	}

	err := cfc.enumer.EnumerateContainers(ctx, cfc.userID, baseID, cfc.addFolder, errs)
	if err != nil {
		return errors.Wrap(err, "enumerating containers")
	}

	if err := cfc.populatePaths(ctx, false); err != nil {
		return errors.Wrap(err, "populating paths")
	}

	return nil
}

func (cfc *contactFolderCache) init(
	ctx context.Context,
	baseNode string,
	baseContainerPath []string,
) error {
	if len(baseNode) == 0 {
		return clues.New("m365 folderID required for base folder").WithClues(ctx)
	}

	if cfc.containerResolver == nil {
		cfc.containerResolver = newContainerResolver()
	}

	return cfc.populateContactRoot(ctx, baseNode, baseContainerPath)
}
