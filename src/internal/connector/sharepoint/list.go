package sharepoint

import (
	"context"
	"sync"

	"github.com/alcionai/clues"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	mssite "github.com/microsoftgraph/msgraph-sdk-go/sites"
	"github.com/pkg/errors"

	"github.com/alcionai/corso/src/internal/common/ptr"
	"github.com/alcionai/corso/src/internal/connector/graph"
	"github.com/alcionai/corso/src/internal/connector/support"
	"github.com/alcionai/corso/src/pkg/fault"
)

type listTuple struct {
	name string
	id   string
}

func preFetchListOptions() *mssite.ItemListsRequestBuilderGetRequestConfiguration {
	selecting := []string{"id", "displayName"}
	queryOptions := mssite.ItemListsRequestBuilderGetQueryParameters{
		Select: selecting,
	}
	options := &mssite.ItemListsRequestBuilderGetRequestConfiguration{
		QueryParameters: &queryOptions,
	}

	return options
}

func preFetchLists(
	ctx context.Context,
	gs graph.Servicer,
	siteID string,
) ([]listTuple, error) {
	var (
		builder    = gs.Client().SitesById(siteID).Lists()
		options    = preFetchListOptions()
		listTuples = make([]listTuple, 0)
	)

	for {
		resp, err := builder.Get(ctx, options)
		if err != nil {
			return nil, clues.Wrap(err, "getting lists").WithClues(ctx).With(graph.ErrData(err)...)
		}

		for _, entry := range resp.GetValue() {
			var (
				id   = ptr.Val(entry.GetId())
				name = ptr.Val(entry.GetDisplayName())
				temp = listTuple{id: id, name: name}
			)

			if len(name) == 0 {
				temp.name = id
			}

			listTuples = append(listTuples, temp)
		}

		if resp.GetOdataNextLink() == nil {
			break
		}

		builder = mssite.NewItemListsRequestBuilder(ptr.Val(resp.GetOdataNextLink()), gs.Adapter())
	}

	return listTuples, nil
}

// list.go contains additional functions to help retrieve SharePoint List data from M365
// SharePoint lists represent lists on a site. Inherits additional properties from
// baseItem: https://learn.microsoft.com/en-us/graph/api/resources/baseitem?view=graph-rest-1.0
// The full details concerning SharePoint Lists can
// be found at: https://learn.microsoft.com/en-us/graph/api/resources/list?view=graph-rest-1.0
// Note additional calls are required for the relationships that exist outside of the object properties.

// loadSiteLists is a utility function to populate a collection of SharePoint.List
// objects associated with a given siteID.
// @param siteID the M365 ID that represents the SharePoint Site
// Makes additional calls to retrieve the following relationships:
// - Columns
// - ContentTypes
// - List Items
func loadSiteLists(
	ctx context.Context,
	gs graph.Servicer,
	siteID string,
	listIDs []string,
	errs *fault.Errors,
) ([]models.Listable, error) {
	var (
		results     = make([]models.Listable, 0)
		semaphoreCh = make(chan struct{}, fetchChannelSize)
		et          = errs.Tracker()
		wg          sync.WaitGroup
		m           sync.Mutex
	)

	defer close(semaphoreCh)

	updateLists := func(list models.Listable) {
		m.Lock()
		defer m.Unlock()

		results = append(results, list)
	}

	for _, listID := range listIDs {
		if et.Err() != nil {
			break
		}

		semaphoreCh <- struct{}{}

		wg.Add(1)

		go func(id string) {
			defer wg.Done()
			defer func() { <-semaphoreCh }()

			var (
				entry models.Listable
				err   error
			)

			entry, err = gs.Client().SitesById(siteID).ListsById(id).Get(ctx, nil)
			if err != nil {
				et.Add(clues.Wrap(err, "getting site list").WithClues(ctx).With(graph.ErrData(err)...))
				return
			}

			cols, cTypes, lItems, err := fetchListContents(ctx, gs, siteID, id, errs)
			if err != nil {
				et.Add(clues.Wrap(err, "getting list contents"))
				return
			}

			entry.SetColumns(cols)
			entry.SetContentTypes(cTypes)
			entry.SetItems(lItems)
			updateLists(entry)
		}(listID)
	}

	wg.Wait()

	return results, et.Err()
}

// fetchListContents utility function to retrieve associated M365 relationships
// which are not included with the standard List query:
// - Columns, ContentTypes, ListItems
func fetchListContents(
	ctx context.Context,
	service graph.Servicer,
	siteID, listID string,
	errs *fault.Errors,
) (
	[]models.ColumnDefinitionable,
	[]models.ContentTypeable,
	[]models.ListItemable,
	error,
) {
	cols, err := fetchColumns(ctx, service, siteID, listID, "")
	if err != nil {
		return nil, nil, nil, err
	}

	cTypes, err := fetchContentTypes(ctx, service, siteID, listID, errs)
	if err != nil {
		return nil, nil, nil, err
	}

	lItems, err := fetchListItems(ctx, service, siteID, listID, errs)
	if err != nil {
		return nil, nil, nil, err
	}

	return cols, cTypes, lItems, nil
}

// fetchListItems utility for retrieving ListItem data and the associated relationship
// data. Additional call append data to the tracked items, and do not create additional collections.
// Additional Call:
// * Fields
func fetchListItems(
	ctx context.Context,
	gs graph.Servicer,
	siteID, listID string,
	errs *fault.Errors,
) ([]models.ListItemable, error) {
	var (
		prefix  = gs.Client().SitesById(siteID).ListsById(listID)
		builder = prefix.Items()
		itms    = make([]models.ListItemable, 0)
		et      = errs.Tracker()
	)

	for {
		if errs.Err() != nil {
			break
		}

		resp, err := builder.Get(ctx, nil)
		if err != nil {
			return nil, errors.Wrap(err, support.ConnectorStackErrorTrace(err))
		}

		for _, itm := range resp.GetValue() {
			if et.Err() != nil {
				break
			}

			newPrefix := prefix.ItemsById(*itm.GetId())

			fields, err := newPrefix.Fields().Get(ctx, nil)
			if err != nil {
				et.Add(clues.Wrap(err, "getting list fields").WithClues(ctx).With(graph.ErrData(err)...))
				continue
			}

			itm.SetFields(fields)

			itms = append(itms, itm)
		}

		if resp.GetOdataNextLink() == nil {
			break
		}

		builder = mssite.NewItemListsItemItemsRequestBuilder(*resp.GetOdataNextLink(), gs.Adapter())
	}

	return itms, et.Err()
}

// fetchColumns utility function to return columns from a site.
// An additional call required to check for details concerning the SourceColumn.
// For additional details:  https://learn.microsoft.com/en-us/graph/api/resources/columndefinition?view=graph-rest-1.0
// TODO: Refactor on if/else (dadams39)
func fetchColumns(
	ctx context.Context,
	gs graph.Servicer,
	siteID, listID, cTypeID string,
) ([]models.ColumnDefinitionable, error) {
	cs := make([]models.ColumnDefinitionable, 0)

	if len(cTypeID) == 0 {
		builder := gs.Client().SitesById(siteID).ListsById(listID).Columns()

		for {
			resp, err := builder.Get(ctx, nil)
			if err != nil {
				return nil, clues.Wrap(err, "getting list columns").WithClues(ctx).With(graph.ErrData(err)...)
			}

			cs = append(cs, resp.GetValue()...)

			if resp.GetOdataNextLink() == nil {
				break
			}

			builder = mssite.NewItemListsItemColumnsRequestBuilder(*resp.GetOdataNextLink(), gs.Adapter())
		}
	} else {
		builder := gs.Client().SitesById(siteID).ListsById(listID).ContentTypesById(cTypeID).Columns()

		for {
			resp, err := builder.Get(ctx, nil)
			if err != nil {
				return nil, clues.Wrap(err, "getting content columns").WithClues(ctx).With(graph.ErrData(err)...)
			}

			cs = append(cs, resp.GetValue()...)

			if resp.GetOdataNextLink() == nil {
				break
			}

			builder = mssite.NewItemListsItemContentTypesItemColumnsRequestBuilder(*resp.GetOdataNextLink(), gs.Adapter())
		}
	}

	return cs, nil
}

// fetchContentTypes retrieves all data for content type. Additional queries required
// for the following:
// - ColumnLinks
// - Columns
// Expand queries not used to retrieve the above. Possibly more than 20.
// Known Limitations: https://learn.microsoft.com/en-us/graph/known-issues#query-parameters
func fetchContentTypes(
	ctx context.Context,
	gs graph.Servicer,
	siteID, listID string,
	errs *fault.Errors,
) ([]models.ContentTypeable, error) {
	var (
		et      = errs.Tracker()
		cTypes  = make([]models.ContentTypeable, 0)
		builder = gs.Client().SitesById(siteID).ListsById(listID).ContentTypes()
	)

	for {
		if errs.Err() != nil {
			break
		}

		resp, err := builder.Get(ctx, nil)
		if err != nil {
			return nil, errors.Wrap(err, support.ConnectorStackErrorTrace(err))
		}

		for _, cont := range resp.GetValue() {
			if et.Err() != nil {
				break
			}

			id := ptr.Val(cont.GetId())

			links, err := fetchColumnLinks(ctx, gs, siteID, listID, id)
			if err != nil {
				et.Add(err)
				continue
			}

			cont.SetColumnLinks(links)

			cs, err := fetchColumns(ctx, gs, siteID, listID, id)
			if err != nil {
				et.Add(err)
				continue
			}

			cont.SetColumns(cs)

			cTypes = append(cTypes, cont)
		}

		if resp.GetOdataNextLink() == nil {
			break
		}

		builder = mssite.NewItemListsItemContentTypesRequestBuilder(*resp.GetOdataNextLink(), gs.Adapter())
	}

	return cTypes, et.Err()
}

func fetchColumnLinks(
	ctx context.Context,
	gs graph.Servicer,
	siteID, listID, cTypeID string,
) ([]models.ColumnLinkable, error) {
	var (
		builder = gs.Client().SitesById(siteID).ListsById(listID).ContentTypesById(cTypeID).ColumnLinks()
		links   = make([]models.ColumnLinkable, 0)
	)

	for {
		resp, err := builder.Get(ctx, nil)
		if err != nil {
			return nil, clues.Wrap(err, "getting column links").WithClues(ctx).With(graph.ErrData(err)...)
		}

		links = append(links, resp.GetValue()...)

		if resp.GetOdataNextLink() == nil {
			break
		}

		builder = mssite.
			NewItemListsItemContentTypesItemColumnLinksRequestBuilder(
				*resp.GetOdataNextLink(),
				gs.Adapter(),
			)
	}

	return links, nil
}

// DeleteList removes a list object from a site.
func DeleteList(
	ctx context.Context,
	gs graph.Servicer,
	siteID, listID string,
) error {
	err := gs.Client().SitesById(siteID).ListsById(listID).Delete(ctx, nil)
	if err != nil {
		return clues.Wrap(err, "deleting list").WithClues(ctx).With(graph.ErrData(err)...)
	}

	return nil
}
