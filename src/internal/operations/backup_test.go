package operations

import (
	"context"
	stdpath "path"
	"testing"
	"time"

	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/alcionai/corso/src/internal/connector/support"
	"github.com/alcionai/corso/src/internal/data"
	evmock "github.com/alcionai/corso/src/internal/events/mock"
	"github.com/alcionai/corso/src/internal/kopia"
	"github.com/alcionai/corso/src/internal/model"
	"github.com/alcionai/corso/src/internal/tester"
	"github.com/alcionai/corso/src/pkg/account"
	"github.com/alcionai/corso/src/pkg/backup"
	"github.com/alcionai/corso/src/pkg/backup/details"
	"github.com/alcionai/corso/src/pkg/control"
	"github.com/alcionai/corso/src/pkg/fault"
	"github.com/alcionai/corso/src/pkg/path"
	"github.com/alcionai/corso/src/pkg/selectors"
	"github.com/alcionai/corso/src/pkg/store"
)

// ---------------------------------------------------------------------------
// mocks
// ---------------------------------------------------------------------------

// ----- restore producer

type mockRestorer struct {
	gotPaths  []path.Path
	colls     []data.RestoreCollection
	collsByID map[string][]data.RestoreCollection // snapshotID: []RestoreCollection
	err       error
	onRestore restoreFunc
}

type restoreFunc func(id string, ps []path.Path) ([]data.RestoreCollection, error)

func (mr *mockRestorer) buildRestoreFunc(
	t *testing.T,
	oid string,
	ops []path.Path,
) {
	mr.onRestore = func(id string, ps []path.Path) ([]data.RestoreCollection, error) {
		assert.Equal(t, oid, id, "manifest id")
		checkPaths(t, ops, ps)

		return mr.colls, mr.err
	}
}

func (mr *mockRestorer) RestoreMultipleItems(
	ctx context.Context,
	snapshotID string,
	paths []path.Path,
	bc kopia.ByteCounter,
	errs *fault.Errors,
) ([]data.RestoreCollection, error) {
	mr.gotPaths = append(mr.gotPaths, paths...)

	if mr.onRestore != nil {
		return mr.onRestore(snapshotID, paths)
	}

	if len(mr.collsByID) > 0 {
		return mr.collsByID[snapshotID], mr.err
	}

	return mr.colls, mr.err
}

func checkPaths(t *testing.T, expected, got []path.Path) {
	assert.ElementsMatch(t, expected, got)
}

// ----- backup producer

type mockBackuper struct {
	checkFunc func(
		bases []kopia.IncrementalBase,
		cs []data.BackupCollection,
		tags map[string]string,
		buildTreeWithBase bool)
}

func (mbu mockBackuper) BackupCollections(
	ctx context.Context,
	bases []kopia.IncrementalBase,
	cs []data.BackupCollection,
	excluded map[string]struct{},
	tags map[string]string,
	buildTreeWithBase bool,
	errs *fault.Errors,
) (*kopia.BackupStats, *details.Builder, map[string]kopia.PrevRefs, error) {
	if mbu.checkFunc != nil {
		mbu.checkFunc(bases, cs, tags, buildTreeWithBase)
	}

	return &kopia.BackupStats{}, &details.Builder{}, nil, nil
}

// ----- details

type mockDetailsReader struct {
	entries map[string]*details.Details
}

func (mdr mockDetailsReader) ReadBackupDetails(
	ctx context.Context,
	detailsID string,
	errs *fault.Errors,
) (*details.Details, error) {
	r := mdr.entries[detailsID]

	if r == nil {
		return nil, errors.Errorf("no details for ID %s", detailsID)
	}

	return r, nil
}

// ----- model store for backups

type mockBackupStorer struct {
	// Only using this to store backup models right now.
	entries map[model.StableID]backup.Backup
}

func (mbs mockBackupStorer) Get(
	ctx context.Context,
	s model.Schema,
	id model.StableID,
	toPopulate model.Model,
) error {
	if s != model.BackupSchema {
		return errors.Errorf("unexpected schema %s", s)
	}

	r, ok := mbs.entries[id]
	if !ok {
		return errors.Errorf("model with id %s not found", id)
	}

	bu, ok := toPopulate.(*backup.Backup)
	if !ok {
		return errors.Errorf("bad input type %T", toPopulate)
	}

	*bu = r

	return nil
}

func (mbs mockBackupStorer) Delete(context.Context, model.Schema, model.StableID) error {
	return errors.New("not implemented")
}

func (mbs mockBackupStorer) DeleteWithModelStoreID(context.Context, manifest.ID) error {
	return errors.New("not implemented")
}

func (mbs mockBackupStorer) GetIDsForType(
	context.Context,
	model.Schema,
	map[string]string,
) ([]*model.BaseModel, error) {
	return nil, errors.New("not implemented")
}

func (mbs mockBackupStorer) GetWithModelStoreID(
	context.Context,
	model.Schema,
	manifest.ID,
	model.Model,
) error {
	return errors.New("not implemented")
}

func (mbs mockBackupStorer) Put(context.Context, model.Schema, model.Model) error {
	return errors.New("not implemented")
}

func (mbs mockBackupStorer) Update(context.Context, model.Schema, model.Model) error {
	return errors.New("not implemented")
}

// ---------------------------------------------------------------------------
// helper funcs
// ---------------------------------------------------------------------------

// expects you to Append your own file
func makeMetadataBasePath(
	t *testing.T,
	tenant string,
	service path.ServiceType,
	resourceOwner string,
	category path.CategoryType,
) path.Path {
	t.Helper()

	p, err := path.Builder{}.ToServiceCategoryMetadataPath(
		tenant,
		resourceOwner,
		service,
		category,
		false)
	require.NoError(t, err)

	return p
}

func makeMetadataPath(
	t *testing.T,
	tenant string,
	service path.ServiceType,
	resourceOwner string,
	category path.CategoryType,
	fileName string,
) path.Path {
	t.Helper()

	p, err := path.Builder{}.Append(fileName).ToServiceCategoryMetadataPath(
		tenant,
		resourceOwner,
		service,
		category,
		true)
	require.NoError(t, err)

	return p
}

func makeFolderEntry(
	t *testing.T,
	pb *path.Builder,
	size int64,
	modTime time.Time,
) *details.DetailsEntry {
	t.Helper()

	return &details.DetailsEntry{
		RepoRef:     pb.String(),
		ShortRef:    pb.ShortRef(),
		ParentRef:   pb.Dir().ShortRef(),
		LocationRef: pb.PopFront().PopFront().PopFront().PopFront().Dir().String(),
		ItemInfo: details.ItemInfo{
			Folder: &details.FolderInfo{
				ItemType:    details.FolderItem,
				DisplayName: pb.Elements()[len(pb.Elements())-1],
				Modified:    modTime,
				Size:        size,
			},
		},
	}
}

// TODO(ashmrtn): Really need to factor a function like this out into some
// common file that is only compiled for tests.
func makePath(t *testing.T, elements []string, isItem bool) path.Path {
	t.Helper()

	p, err := path.FromDataLayerPath(stdpath.Join(elements...), isItem)
	require.NoError(t, err)

	return p
}

func makeDetailsEntry(
	t *testing.T,
	p path.Path,
	l path.Path,
	size int,
	updated bool,
) *details.DetailsEntry {
	t.Helper()

	var lr string
	if l != nil {
		lr = l.PopFront().PopFront().PopFront().PopFront().Dir().String()
	}

	res := &details.DetailsEntry{
		RepoRef:     p.String(),
		ShortRef:    p.ShortRef(),
		ParentRef:   p.ToBuilder().Dir().ShortRef(),
		LocationRef: lr,
		ItemInfo:    details.ItemInfo{},
		Updated:     updated,
	}

	switch p.Service() {
	case path.ExchangeService:
		if p.Category() != path.EmailCategory {
			assert.FailNowf(
				t,
				"category %s not supported in helper function",
				p.Category().String(),
			)
		}

		res.Exchange = &details.ExchangeInfo{
			ItemType: details.ExchangeMail,
			Size:     int64(size),
		}

	case path.OneDriveService:
		parent, err := path.GetDriveFolderPath(p)
		require.NoError(t, err)

		res.OneDrive = &details.OneDriveInfo{
			ItemType:   details.OneDriveItem,
			ParentPath: parent,
			Size:       int64(size),
		}

	default:
		assert.FailNowf(
			t,
			"service %s not supported in helper function",
			p.Service().String(),
		)
	}

	return res
}

// TODO(ashmrtn): This should belong to some code that lives in the kopia
// package that is only compiled when running tests.
func makeKopiaTagKey(k string) string {
	return "tag:" + k
}

func makeManifest(t *testing.T, backupID model.StableID, incompleteReason string) *snapshot.Manifest {
	t.Helper()

	tagKey := makeKopiaTagKey(kopia.TagBackupID)

	return &snapshot.Manifest{
		Tags: map[string]string{
			tagKey: string(backupID),
		},
		IncompleteReason: incompleteReason,
	}
}

// ---------------------------------------------------------------------------
// unit tests
// ---------------------------------------------------------------------------

type BackupOpSuite struct {
	suite.Suite
}

func TestBackupOpSuite(t *testing.T) {
	suite.Run(t, new(BackupOpSuite))
}

func (suite *BackupOpSuite) TestBackupOperation_PersistResults() {
	ctx, flush := tester.NewContext()
	defer flush()

	var (
		kw   = &kopia.Wrapper{}
		sw   = &store.Wrapper{}
		acct = account.Account{}
		now  = time.Now()
	)

	table := []struct {
		expectStatus opStatus
		expectErr    assert.ErrorAssertionFunc
		stats        backupStats
	}{
		{
			expectStatus: Completed,
			expectErr:    assert.NoError,
			stats: backupStats{
				resourceCount: 1,
				k: &kopia.BackupStats{
					TotalFileCount:     1,
					TotalHashedBytes:   1,
					TotalUploadedBytes: 1,
				},
				gc: &support.ConnectorOperationStatus{
					Successful: 1,
				},
			},
		},
		{
			expectStatus: Failed,
			expectErr:    assert.Error,
			stats: backupStats{
				readErr: assert.AnError,
				k:       &kopia.BackupStats{},
				gc:      &support.ConnectorOperationStatus{},
			},
		},
		{
			expectStatus: NoData,
			expectErr:    assert.NoError,
			stats: backupStats{
				k:  &kopia.BackupStats{},
				gc: &support.ConnectorOperationStatus{},
			},
		},
	}
	for _, test := range table {
		suite.T().Run(test.expectStatus.String(), func(t *testing.T) {
			sel := selectors.Selector{}
			sel.DiscreteOwner = "bombadil"

			op, err := NewBackupOperation(
				ctx,
				control.Options{},
				kw,
				sw,
				acct,
				sel,
				evmock.NewBus())
			require.NoError(t, err)
			test.expectErr(t, op.persistResults(now, &test.stats))

			assert.Equal(t, test.expectStatus.String(), op.Status.String(), "status")
			assert.Equal(t, test.stats.gc.Successful, op.Results.ItemsRead, "items read")
			assert.Equal(t, test.stats.k.TotalFileCount, op.Results.ItemsWritten, "items written")
			assert.Equal(t, test.stats.k.TotalHashedBytes, op.Results.BytesRead, "bytes read")
			assert.Equal(t, test.stats.k.TotalUploadedBytes, op.Results.BytesUploaded, "bytes written")
			assert.Equal(t, test.stats.resourceCount, op.Results.ResourceOwners, "resource owners")
			assert.Equal(t, test.stats.readErr, op.Results.ReadErrors, "read errors")
			assert.Equal(t, test.stats.writeErr, op.Results.WriteErrors, "write errors")
			assert.Equal(t, now, op.Results.StartedAt, "started at")
			assert.Less(t, now, op.Results.CompletedAt, "completed at")
		})
	}
}

func (suite *BackupOpSuite) TestBackupOperation_ConsumeBackupDataCollections_Paths() {
	var (
		tenant        = "a-tenant"
		resourceOwner = "a-user"

		emailBuilder = path.Builder{}.Append(
			tenant,
			path.ExchangeService.String(),
			resourceOwner,
			path.EmailCategory.String(),
		)
		contactsBuilder = path.Builder{}.Append(
			tenant,
			path.ExchangeService.String(),
			resourceOwner,
			path.ContactsCategory.String(),
		)

		emailReason = kopia.Reason{
			ResourceOwner: resourceOwner,
			Service:       path.ExchangeService,
			Category:      path.EmailCategory,
		}
		contactsReason = kopia.Reason{
			ResourceOwner: resourceOwner,
			Service:       path.ExchangeService,
			Category:      path.ContactsCategory,
		}

		manifest1 = &snapshot.Manifest{
			ID: "id1",
		}
		manifest2 = &snapshot.Manifest{
			ID: "id2",
		}
	)

	table := []struct {
		name     string
		inputMan []*kopia.ManifestEntry
		expected []kopia.IncrementalBase
	}{
		{
			name: "SingleManifestSingleReason",
			inputMan: []*kopia.ManifestEntry{
				{
					Manifest: manifest1,
					Reasons: []kopia.Reason{
						emailReason,
					},
				},
			},
			expected: []kopia.IncrementalBase{
				{
					Manifest: manifest1,
					SubtreePaths: []*path.Builder{
						emailBuilder,
					},
				},
			},
		},
		{
			name: "SingleManifestMultipleReasons",
			inputMan: []*kopia.ManifestEntry{
				{
					Manifest: manifest1,
					Reasons: []kopia.Reason{
						emailReason,
						contactsReason,
					},
				},
			},
			expected: []kopia.IncrementalBase{
				{
					Manifest: manifest1,
					SubtreePaths: []*path.Builder{
						emailBuilder,
						contactsBuilder,
					},
				},
			},
		},
		{
			name: "MultipleManifestsMultipleReasons",
			inputMan: []*kopia.ManifestEntry{
				{
					Manifest: manifest1,
					Reasons: []kopia.Reason{
						emailReason,
						contactsReason,
					},
				},
				{
					Manifest: manifest2,
					Reasons: []kopia.Reason{
						emailReason,
						contactsReason,
					},
				},
			},
			expected: []kopia.IncrementalBase{
				{
					Manifest: manifest1,
					SubtreePaths: []*path.Builder{
						emailBuilder,
						contactsBuilder,
					},
				},
				{
					Manifest: manifest2,
					SubtreePaths: []*path.Builder{
						emailBuilder,
						contactsBuilder,
					},
				},
			},
		},
	}

	for _, test := range table {
		suite.T().Run(test.name, func(t *testing.T) {
			ctx, flush := tester.NewContext()
			defer flush()

			mbu := &mockBackuper{
				checkFunc: func(
					bases []kopia.IncrementalBase,
					cs []data.BackupCollection,
					tags map[string]string,
					buildTreeWithBase bool,
				) {
					assert.ElementsMatch(t, test.expected, bases)
				},
			}

			//nolint:errcheck
			consumeBackupDataCollections(
				ctx,
				mbu,
				tenant,
				nil,
				test.inputMan,
				nil,
				nil,
				model.StableID(""),
				true,
				fault.New(true))
		})
	}
}

func (suite *BackupOpSuite) TestBackupOperation_MergeBackupDetails_AddsItems() {
	var (
		tenant = "a-tenant"
		ro     = "a-user"

		itemPath1 = makePath(
			suite.T(),
			[]string{
				tenant,
				path.OneDriveService.String(),
				ro,
				path.FilesCategory.String(),
				"drives",
				"drive-id",
				"root:",
				"work",
				"item1",
			},
			true,
		)
		locationPath1 = makePath(
			suite.T(),
			[]string{
				tenant,
				path.OneDriveService.String(),
				ro,
				path.FilesCategory.String(),
				"drives",
				"drive-id",
				"root:",
				"work-display-name",
				"item1",
			},
			true,
		)
		itemPath2 = makePath(
			suite.T(),
			[]string{
				tenant,
				path.OneDriveService.String(),
				ro,
				path.FilesCategory.String(),
				"drives",
				"drive-id",
				"root:",
				"personal",
				"item2",
			},
			true,
		)
		locationPath2 = makePath(
			suite.T(),
			[]string{
				tenant,
				path.OneDriveService.String(),
				ro,
				path.FilesCategory.String(),
				"drives",
				"drive-id",
				"root:",
				"personal-display-name",
				"item2",
			},
			true,
		)
		itemPath3 = makePath(
			suite.T(),
			[]string{
				tenant,
				path.ExchangeService.String(),
				ro,
				path.EmailCategory.String(),
				"personal",
				"item3",
			},
			true,
		)
		locationPath3 = makePath(
			suite.T(),
			[]string{
				tenant,
				path.ExchangeService.String(),
				ro,
				path.EmailCategory.String(),
				"personal-display-name",
				"item3",
			},
			true,
		)

		backup1 = backup.Backup{
			BaseModel: model.BaseModel{
				ID: "bid1",
			},
			DetailsID: "did1",
		}

		backup2 = backup.Backup{
			BaseModel: model.BaseModel{
				ID: "bid2",
			},
			DetailsID: "did2",
		}

		pathReason1 = kopia.Reason{
			ResourceOwner: itemPath1.ResourceOwner(),
			Service:       itemPath1.Service(),
			Category:      itemPath1.Category(),
		}
		pathReason3 = kopia.Reason{
			ResourceOwner: itemPath3.ResourceOwner(),
			Service:       itemPath3.Service(),
			Category:      itemPath3.Category(),
		}
	)

	itemParents1, err := path.GetDriveFolderPath(itemPath1)
	require.NoError(suite.T(), err)

	table := []struct {
		name                         string
		populatedModels              map[model.StableID]backup.Backup
		populatedDetails             map[string]*details.Details
		inputMans                    []*kopia.ManifestEntry
		inputShortRefsFromPrevBackup map[string]kopia.PrevRefs

		errCheck        assert.ErrorAssertionFunc
		expectedEntries []*details.DetailsEntry
	}{
		{
			name:     "NilShortRefsFromPrevBackup",
			errCheck: assert.NoError,
			// Use empty slice so we don't error out on nil != empty.
			expectedEntries: []*details.DetailsEntry{},
		},
		{
			name:                         "EmptyShortRefsFromPrevBackup",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{},
			errCheck:                     assert.NoError,
			// Use empty slice so we don't error out on nil != empty.
			expectedEntries: []*details.DetailsEntry{},
		},
		{
			name: "BackupIDNotFound",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo:     itemPath1,
					Location: locationPath1,
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), "foo", ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
			},
			errCheck: assert.Error,
		},
		{
			name: "DetailsIDNotFound",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo:     itemPath1,
					Location: locationPath1,
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
			},
			populatedModels: map[model.StableID]backup.Backup{
				backup1.ID: {
					BaseModel: model.BaseModel{
						ID: backup1.ID,
					},
					DetailsID: "foo",
				},
			},
			errCheck: assert.Error,
		},
		{
			name: "BaseMissingItems",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo:     itemPath1,
					Location: locationPath1,
				},
				itemPath2.ShortRef(): {
					Repo:     itemPath2,
					Location: locationPath2,
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
			},
			populatedModels: map[model.StableID]backup.Backup{
				backup1.ID: backup1,
			},
			populatedDetails: map[string]*details.Details{
				backup1.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							*makeDetailsEntry(suite.T(), itemPath1, itemPath1, 42, false),
						},
					},
				},
			},
			errCheck: assert.Error,
		},
		{
			name: "TooManyItems",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo:     itemPath1,
					Location: locationPath1,
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
			},
			populatedModels: map[model.StableID]backup.Backup{
				backup1.ID: backup1,
			},
			populatedDetails: map[string]*details.Details{
				backup1.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							*makeDetailsEntry(suite.T(), itemPath1, itemPath1, 42, false),
						},
					},
				},
			},
			errCheck: assert.Error,
		},
		{
			name: "BadBaseRepoRef",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo:     itemPath2,
					Location: locationPath2,
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
			},
			populatedModels: map[model.StableID]backup.Backup{
				backup1.ID: backup1,
			},
			populatedDetails: map[string]*details.Details{
				backup1.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							{
								RepoRef: stdpath.Join(
									append(
										[]string{
											itemPath1.Tenant(),
											itemPath1.Service().String(),
											itemPath1.ResourceOwner(),
											path.UnknownCategory.String(),
										},
										itemPath1.Folders()...,
									)...,
								),
								ItemInfo: details.ItemInfo{
									OneDrive: &details.OneDriveInfo{
										ItemType:   details.OneDriveItem,
										ParentPath: itemParents1,
										Size:       42,
									},
								},
							},
						},
					},
				},
			},
			errCheck: assert.Error,
		},
		{
			name: "BadOneDrivePath",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo: makePath(
						suite.T(),
						[]string{
							itemPath1.Tenant(),
							path.OneDriveService.String(),
							itemPath1.ResourceOwner(),
							path.FilesCategory.String(),
							"personal",
							"item1",
						},
						true,
					),
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
			},
			populatedModels: map[model.StableID]backup.Backup{
				backup1.ID: backup1,
			},
			populatedDetails: map[string]*details.Details{
				backup1.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							*makeDetailsEntry(suite.T(), itemPath1, itemPath1, 42, false),
						},
					},
				},
			},
			errCheck: assert.Error,
		},
		{
			name: "ItemMerged",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo:     itemPath1,
					Location: locationPath1,
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
			},
			populatedModels: map[model.StableID]backup.Backup{
				backup1.ID: backup1,
			},
			populatedDetails: map[string]*details.Details{
				backup1.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							*makeDetailsEntry(suite.T(), itemPath1, locationPath1, 42, false),
						},
					},
				},
			},
			errCheck: assert.NoError,
			expectedEntries: []*details.DetailsEntry{
				makeDetailsEntry(suite.T(), itemPath1, locationPath1, 42, false),
			},
		},
		{
			name: "ItemMergedNoLocation",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo: itemPath1,
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
			},
			populatedModels: map[model.StableID]backup.Backup{
				backup1.ID: backup1,
			},
			populatedDetails: map[string]*details.Details{
				backup1.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							*makeDetailsEntry(suite.T(), itemPath1, nil, 42, false),
						},
					},
				},
			},
			errCheck: assert.NoError,
			expectedEntries: []*details.DetailsEntry{
				makeDetailsEntry(suite.T(), itemPath1, nil, 42, false),
			},
		},
		{
			name: "ItemMergedSameLocation",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo:     itemPath1,
					Location: itemPath1,
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
			},
			populatedModels: map[model.StableID]backup.Backup{
				backup1.ID: backup1,
			},
			populatedDetails: map[string]*details.Details{
				backup1.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							*makeDetailsEntry(suite.T(), itemPath1, itemPath1, 42, false),
						},
					},
				},
			},
			errCheck: assert.NoError,
			expectedEntries: []*details.DetailsEntry{
				makeDetailsEntry(suite.T(), itemPath1, itemPath1, 42, false),
			},
		},
		{
			name: "ItemMergedExtraItemsInBase",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo:     itemPath1,
					Location: locationPath1,
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
			},
			populatedModels: map[model.StableID]backup.Backup{
				backup1.ID: backup1,
			},
			populatedDetails: map[string]*details.Details{
				backup1.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							*makeDetailsEntry(suite.T(), itemPath1, locationPath1, 42, false),
							*makeDetailsEntry(suite.T(), itemPath2, locationPath2, 84, false),
						},
					},
				},
			},
			errCheck: assert.NoError,
			expectedEntries: []*details.DetailsEntry{
				makeDetailsEntry(suite.T(), itemPath1, locationPath1, 42, false),
			},
		},
		{
			name: "ItemMoved",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo:     itemPath2,
					Location: locationPath2,
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
			},
			populatedModels: map[model.StableID]backup.Backup{
				backup1.ID: backup1,
			},
			populatedDetails: map[string]*details.Details{
				backup1.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							*makeDetailsEntry(suite.T(), itemPath1, locationPath1, 42, false),
						},
					},
				},
			},
			errCheck: assert.NoError,
			expectedEntries: []*details.DetailsEntry{
				makeDetailsEntry(suite.T(), itemPath2, locationPath2, 42, true),
			},
		},
		{
			name: "MultipleBases",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo:     itemPath1,
					Location: locationPath1,
				},
				itemPath3.ShortRef(): {
					Repo:     itemPath3,
					Location: locationPath3,
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
				{
					Manifest: makeManifest(suite.T(), backup2.ID, ""),
					Reasons: []kopia.Reason{
						pathReason3,
					},
				},
			},
			populatedModels: map[model.StableID]backup.Backup{
				backup1.ID: backup1,
				backup2.ID: backup2,
			},
			populatedDetails: map[string]*details.Details{
				backup1.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							*makeDetailsEntry(suite.T(), itemPath1, locationPath1, 42, false),
						},
					},
				},
				backup2.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							// This entry should not be picked due to a mismatch on Reasons.
							*makeDetailsEntry(suite.T(), itemPath1, locationPath1, 84, false),
							// This item should be picked.
							*makeDetailsEntry(suite.T(), itemPath3, locationPath3, 37, false),
						},
					},
				},
			},
			errCheck: assert.NoError,
			expectedEntries: []*details.DetailsEntry{
				makeDetailsEntry(suite.T(), itemPath1, locationPath1, 42, false),
				makeDetailsEntry(suite.T(), itemPath3, locationPath3, 37, false),
			},
		},
		{
			name: "SomeBasesIncomplete",
			inputShortRefsFromPrevBackup: map[string]kopia.PrevRefs{
				itemPath1.ShortRef(): {
					Repo:     itemPath1,
					Location: locationPath1,
				},
			},
			inputMans: []*kopia.ManifestEntry{
				{
					Manifest: makeManifest(suite.T(), backup1.ID, ""),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
				{
					Manifest: makeManifest(suite.T(), backup2.ID, "checkpoint"),
					Reasons: []kopia.Reason{
						pathReason1,
					},
				},
			},
			populatedModels: map[model.StableID]backup.Backup{
				backup1.ID: backup1,
				backup2.ID: backup2,
			},
			populatedDetails: map[string]*details.Details{
				backup1.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							*makeDetailsEntry(suite.T(), itemPath1, locationPath1, 42, false),
						},
					},
				},
				backup2.DetailsID: {
					DetailsModel: details.DetailsModel{
						Entries: []details.DetailsEntry{
							// This entry should not be picked due to being incomplete.
							*makeDetailsEntry(suite.T(), itemPath1, locationPath1, 84, false),
						},
					},
				},
			},
			errCheck: assert.NoError,
			expectedEntries: []*details.DetailsEntry{
				makeDetailsEntry(suite.T(), itemPath1, locationPath1, 42, false),
			},
		},
	}

	for _, test := range table {
		suite.T().Run(test.name, func(t *testing.T) {
			ctx, flush := tester.NewContext()
			defer flush()

			mdr := mockDetailsReader{entries: test.populatedDetails}
			w := &store.Wrapper{Storer: mockBackupStorer{entries: test.populatedModels}}
			deets := details.Builder{}

			err := mergeDetails(
				ctx,
				w,
				mdr,
				test.inputMans,
				test.inputShortRefsFromPrevBackup,
				&deets,
				fault.New(true))
			test.errCheck(t, err)

			if err != nil {
				return
			}

			assert.ElementsMatch(t, test.expectedEntries, deets.Details().Items())
		})
	}
}

func (suite *BackupOpSuite) TestBackupOperation_MergeBackupDetails_AddsFolders() {
	var (
		t = suite.T()

		tenant = "a-tenant"
		ro     = "a-user"

		pathElems = []string{
			tenant,
			path.ExchangeService.String(),
			ro,
			path.EmailCategory.String(),
			"work",
			"item1",
		}

		itemPath1 = makePath(
			t,
			pathElems,
			true)

		locPath1 = makePath(
			t,
			pathElems[:len(pathElems)-1],
			false)

		backup1 = backup.Backup{
			BaseModel: model.BaseModel{
				ID: "bid1",
			},
			DetailsID: "did1",
		}

		pathReason1 = kopia.Reason{
			ResourceOwner: itemPath1.ResourceOwner(),
			Service:       itemPath1.Service(),
			Category:      itemPath1.Category(),
		}

		inputToMerge = map[string]kopia.PrevRefs{
			itemPath1.ShortRef(): {
				Repo:     itemPath1,
				Location: locPath1,
			},
		}

		inputMans = []*kopia.ManifestEntry{
			{
				Manifest: makeManifest(t, backup1.ID, ""),
				Reasons: []kopia.Reason{
					pathReason1,
				},
			},
		}

		populatedModels = map[model.StableID]backup.Backup{
			backup1.ID: backup1,
		}

		itemSize    = 42
		itemDetails = makeDetailsEntry(t, itemPath1, itemPath1, itemSize, false)

		populatedDetails = map[string]*details.Details{
			backup1.DetailsID: {
				DetailsModel: details.DetailsModel{
					Entries: []details.DetailsEntry{
						*itemDetails,
					},
				},
			},
		}

		expectedEntries = []details.DetailsEntry{
			*itemDetails,
		}
	)

	itemDetails.Exchange.Modified = time.Now()

	for i := 1; i < len(pathElems); i++ {
		expectedEntries = append(expectedEntries, *makeFolderEntry(
			t,
			path.Builder{}.Append(pathElems[:i]...),
			int64(itemSize),
			itemDetails.Exchange.Modified,
		))
	}

	ctx, flush := tester.NewContext()
	defer flush()

	mdr := mockDetailsReader{entries: populatedDetails}
	w := &store.Wrapper{Storer: mockBackupStorer{entries: populatedModels}}
	deets := details.Builder{}

	err := mergeDetails(
		ctx,
		w,
		mdr,
		inputMans,
		inputToMerge,
		&deets,
		fault.New(true))
	assert.NoError(t, err)
	assert.ElementsMatch(t, expectedEntries, deets.Details().Entries)
}
