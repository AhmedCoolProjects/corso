// Package onedrive provides support for retrieving M365 OneDrive objects
package onedrive

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alcionai/clues"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/pkg/errors"
	"github.com/spatialcurrent/go-lazy/pkg/lazy"

	"github.com/alcionai/corso/src/internal/connector/graph"
	"github.com/alcionai/corso/src/internal/connector/support"
	"github.com/alcionai/corso/src/internal/data"
	"github.com/alcionai/corso/src/internal/observe"
	"github.com/alcionai/corso/src/pkg/backup/details"
	"github.com/alcionai/corso/src/pkg/control"
	"github.com/alcionai/corso/src/pkg/fault"
	"github.com/alcionai/corso/src/pkg/logger"
	"github.com/alcionai/corso/src/pkg/path"
)

const (
	// TODO: This number needs to be tuned
	// Consider max open file limit `ulimit -n`, usually 1024 when setting this value
	collectionChannelBufferSize = 5

	// TODO: Tune this later along with collectionChannelBufferSize
	urlPrefetchChannelBufferSize = 5

	MetaFileSuffix    = ".meta"
	DirMetaFileSuffix = ".dirmeta"
	DataFileSuffix    = ".data"
)

var (
	_ data.BackupCollection = &Collection{}
	_ data.Stream           = &Item{}
	_ data.StreamInfo       = &Item{}
	_ data.StreamModTime    = &Item{}
)

// Collection represents a set of OneDrive objects retrieved from M365
type Collection struct {
	// configured to handle large item downloads
	itemClient *http.Client

	// data is used to share data streams with the collection consumer
	data chan data.Stream
	// folderPath indicates what level in the hierarchy this collection
	// represents
	folderPath path.Path
	// M365 IDs of file items within this collection
	driveItems map[string]models.DriveItemable
	// M365 ID of the drive this collection was created from
	driveID        string
	source         driveSource
	service        graph.Servicer
	statusUpdater  support.StatusUpdater
	itemReader     itemReaderFunc
	itemMetaReader itemMetaReaderFunc
	ctrl           control.Options

	// PrevPath is the previous hierarchical path used by this collection.
	// It may be the same as fullPath, if the folder was not renamed or
	// moved.  It will be empty on its first retrieval.
	prevPath path.Path

	// Specifies if it new, moved/rename or deleted
	state data.CollectionState

	// should only be true if the old delta token expired
	doNotMergeItems bool
}

// itemReadFunc returns a reader for the specified item
type itemReaderFunc func(
	hc *http.Client,
	item models.DriveItemable,
) (itemInfo details.ItemInfo, itemData io.ReadCloser, err error)

// itemMetaReaderFunc returns a reader for the metadata of the
// specified item
type itemMetaReaderFunc func(
	ctx context.Context,
	service graph.Servicer,
	driveID string,
	item models.DriveItemable,
	fetchPermissions bool,
) (io.ReadCloser, int, error)

// NewCollection creates a Collection
func NewCollection(
	itemClient *http.Client,
	folderPath path.Path,
	prevPath path.Path,
	driveID string,
	service graph.Servicer,
	statusUpdater support.StatusUpdater,
	source driveSource,
	ctrlOpts control.Options,
	doNotMergeItems bool,
) *Collection {
	c := &Collection{
		itemClient:      itemClient,
		folderPath:      folderPath,
		prevPath:        prevPath,
		driveItems:      map[string]models.DriveItemable{},
		driveID:         driveID,
		source:          source,
		service:         service,
		data:            make(chan data.Stream, collectionChannelBufferSize),
		statusUpdater:   statusUpdater,
		ctrl:            ctrlOpts,
		state:           data.StateOf(prevPath, folderPath),
		doNotMergeItems: doNotMergeItems,
	}

	// Allows tests to set a mock populator
	switch source {
	case SharePointSource:
		c.itemReader = sharePointItemReader
	default:
		c.itemReader = oneDriveItemReader
		c.itemMetaReader = oneDriveItemMetaReader
	}

	return c
}

// Adds an itemID to the collection.  This will make it eligible to be
// populated. The return values denotes if the item was previously
// present or is new one.
func (oc *Collection) Add(item models.DriveItemable) bool {
	_, found := oc.driveItems[*item.GetId()]
	oc.driveItems[*item.GetId()] = item

	return !found // !found = new
}

// Remove removes a item from the collection
func (oc *Collection) Remove(item models.DriveItemable) bool {
	_, found := oc.driveItems[*item.GetId()]
	if !found {
		return false
	}

	delete(oc.driveItems, *item.GetId())

	return true
}

// IsEmpty check if a collection does not contain any items
// TODO(meain): Should we just have function that returns driveItems?
func (oc *Collection) IsEmpty() bool {
	return len(oc.driveItems) == 0
}

// Items() returns the channel containing M365 Exchange objects
func (oc *Collection) Items(
	ctx context.Context,
	errs *fault.Errors, // TODO: currently unused while onedrive isn't up to date with clues/fault
) <-chan data.Stream {
	go oc.populateItems(ctx)
	return oc.data
}

func (oc *Collection) FullPath() path.Path {
	return oc.folderPath
}

func (oc Collection) PreviousPath() path.Path {
	return oc.prevPath
}

func (oc *Collection) SetFullPath(curPath path.Path) {
	oc.folderPath = curPath
	oc.state = data.StateOf(oc.prevPath, curPath)
}

func (oc Collection) State() data.CollectionState {
	return oc.state
}

func (oc Collection) DoNotMergeItems() bool {
	return oc.doNotMergeItems
}

// FilePermission is used to store permissions of a specific user to a
// OneDrive item.
type UserPermission struct {
	ID         string     `json:"id,omitempty"`
	Roles      []string   `json:"role,omitempty"`
	Email      string     `json:"email,omitempty"`
	Expiration *time.Time `json:"expiration,omitempty"`
}

// ItemMeta contains metadata about the Item. It gets stored in a
// separate file in kopia
type Metadata struct {
	FileName    string           `json:"filename,omitempty"`
	Permissions []UserPermission `json:"permissions,omitempty"`
}

// Item represents a single item retrieved from OneDrive
type Item struct {
	id   string
	data io.ReadCloser
	info details.ItemInfo

	// true if the item was marked by graph as deleted.
	deleted bool
}

func (od *Item) UUID() string {
	return od.id
}

func (od *Item) ToReader() io.ReadCloser {
	return od.data
}

// TODO(ashmrtn): Fill in once delta tokens return deleted items.
func (od Item) Deleted() bool {
	return od.deleted
}

func (od *Item) Info() details.ItemInfo {
	return od.info
}

func (od *Item) ModTime() time.Time {
	return od.info.Modified()
}

// populateItems iterates through items added to the collection
// and uses the collection `itemReader` to read the item
func (oc *Collection) populateItems(ctx context.Context) {
	var (
		errs       error
		byteCount  int64
		itemsRead  int64
		dirsRead   int64
		itemsFound int64
		dirsFound  int64
		wg         sync.WaitGroup
		m          sync.Mutex
	)

	// Retrieve the OneDrive folder path to set later in
	// `details.OneDriveInfo`
	parentPathString, err := path.GetDriveFolderPath(oc.folderPath)
	if err != nil {
		oc.reportAsCompleted(ctx, 0, 0, 0, err)
		return
	}

	folderProgress, colCloser := observe.ProgressWithCount(
		ctx,
		observe.ItemQueueMsg,
		observe.PII("/"+parentPathString),
		int64(len(oc.driveItems)))
	defer colCloser()
	defer close(folderProgress)

	semaphoreCh := make(chan struct{}, urlPrefetchChannelBufferSize)
	defer close(semaphoreCh)

	errUpdater := func(id string, err error) {
		m.Lock()
		errs = support.WrapAndAppend(id, err, errs)
		m.Unlock()
	}

	for _, item := range oc.driveItems {
		if oc.ctrl.FailFast && errs != nil {
			break
		}

		semaphoreCh <- struct{}{}

		wg.Add(1)

		go func(item models.DriveItemable) {
			defer wg.Done()
			defer func() { <-semaphoreCh }()

			// Read the item
			var (
				itemID       = *item.GetId()
				itemName     = *item.GetName()
				itemSize     = *item.GetSize()
				itemInfo     details.ItemInfo
				itemMeta     io.ReadCloser
				itemMetaSize int
				metaSuffix   string
				err          error
			)

			isFile := item.GetFile() != nil

			if isFile {
				atomic.AddInt64(&itemsFound, 1)

				metaSuffix = MetaFileSuffix
			} else {
				atomic.AddInt64(&dirsFound, 1)

				metaSuffix = DirMetaFileSuffix
			}

			if oc.source == OneDriveSource {
				// Fetch metadata for the file
				itemMeta, itemMetaSize, err = oc.itemMetaReader(
					ctx,
					oc.service,
					oc.driveID,
					item,
					oc.ctrl.ToggleFeatures.EnablePermissionsBackup)

				if err != nil {
					errUpdater(itemID, clues.Wrap(err, "getting item metadata"))
					return
				}
			}

			switch oc.source {
			case SharePointSource:
				itemInfo.SharePoint = sharePointItemInfo(item, itemSize)
				itemInfo.SharePoint.ParentPath = parentPathString
			default:
				itemInfo.OneDrive = oneDriveItemInfo(item, itemSize)
				itemInfo.OneDrive.ParentPath = parentPathString
			}

			if isFile {
				dataSuffix := ""
				if oc.source == OneDriveSource {
					dataSuffix = DataFileSuffix
				}

				// Construct a new lazy readCloser to feed to the collection consumer.
				// This ensures that downloads won't be attempted unless that consumer
				// attempts to read bytes.  Assumption is that kopia will check things
				// like file modtimes before attempting to read.
				itemReader := lazy.NewLazyReadCloser(func() (io.ReadCloser, error) {
					// Read the item
					var (
						itemData io.ReadCloser
						err      error
					)

					_, itemData, err = oc.itemReader(oc.itemClient, item)

					if err != nil && graph.IsErrUnauthorized(err) {
						// assume unauthorized requests are a sign of an expired
						// jwt token, and that we've overrun the available window
						// to download the actual file.  Re-downloading the item
						// will refresh that download url.
						di, diErr := getDriveItem(ctx, oc.service, oc.driveID, itemID)
						if diErr != nil {
							err = errors.Wrap(diErr, "retrieving expired item")
						}
						item = di
					}

					// check for errors following retries
					if err != nil {
						errUpdater(itemID, err)
						return nil, err
					}

					// display/log the item download
					progReader, closer := observe.ItemProgress(
						ctx,
						itemData,
						observe.ItemBackupMsg,
						observe.PII(itemName+dataSuffix),
						itemSize,
					)
					go closer()

					return progReader, nil
				})

				oc.data <- &Item{
					id:   itemName + dataSuffix,
					data: itemReader,
					info: itemInfo,
				}
			}

			if oc.source == OneDriveSource {
				metaReader := lazy.NewLazyReadCloser(func() (io.ReadCloser, error) {
					progReader, closer := observe.ItemProgress(
						ctx, itemMeta, observe.ItemBackupMsg,
						observe.PII(itemName+metaSuffix), int64(itemMetaSize))
					go closer()
					return progReader, nil
				})

				// TODO(meain): Remove this once we change to always
				// backing up permissions. Until then we cannot rely
				// on whether the previous data is what we need as the
				// user might have not backup up permissions in the
				// previous run.
				metaItemInfo := details.ItemInfo{}
				metaItemInfo.OneDrive = &details.OneDriveInfo{
					Created:    itemInfo.OneDrive.Created,
					ItemName:   itemInfo.OneDrive.ItemName,
					DriveName:  itemInfo.OneDrive.DriveName,
					ItemType:   itemInfo.OneDrive.ItemType,
					IsMeta:     true,
					Modified:   time.Now(), // set to current time to always refresh
					Owner:      itemInfo.OneDrive.Owner,
					ParentPath: itemInfo.OneDrive.ParentPath,
					Size:       itemInfo.OneDrive.Size,
				}

				oc.data <- &Item{
					id:   itemName + metaSuffix,
					data: metaReader,
					info: metaItemInfo,
				}
			}

			// Item read successfully, add to collection
			if isFile {
				atomic.AddInt64(&itemsRead, 1)
			} else {
				atomic.AddInt64(&dirsRead, 1)
			}

			// byteCount iteration
			atomic.AddInt64(&byteCount, itemSize)

			folderProgress <- struct{}{}
		}(item)
	}

	wg.Wait()

	oc.reportAsCompleted(ctx, int(itemsFound), int(itemsRead), byteCount, errs)
}

func (oc *Collection) reportAsCompleted(ctx context.Context, itemsFound, itemsRead int, byteCount int64, errs error) {
	close(oc.data)

	status := support.CreateStatus(ctx, support.Backup,
		1, // num folders (always 1)
		support.CollectionMetrics{
			Objects:    itemsFound, // items to read,
			Successes:  itemsRead,  // items read successfully,
			TotalBytes: byteCount,  // Number of bytes read in the operation,
		},
		errs,
		oc.folderPath.Folder(false), // Additional details
	)
	logger.Ctx(ctx).Debugw("done streaming items", "status", status.String())
	oc.statusUpdater(status)
}
