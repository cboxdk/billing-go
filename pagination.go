package cboxbilling

// Hand-written (not generated): cursor pagination over the list endpoints.

import "iter"

// CursorPage is what every generated list response model provides.
type CursorPage[T any] interface {
	GetHasMore() bool
	GetNextCursor() string
	GetData() []T
}

// Paginate yields every item of a cursor-paginated list, fetching the next page only when the
// loop needs it. fetch receives "" for the first page and the previous page's next_cursor after.
// An error ends the sequence after it is yielded.
//
//	pages := cboxbilling.Paginate[cboxbilling.Invoice](func(cursor string) (*cboxbilling.InvoiceList, error) {
//		req := client.InvoicesAPI.ListInvoices(ctx, "org_acme")
//		if cursor != "" {
//			req = req.Cursor(cursor)
//		}
//		page, _, err := req.Execute()
//		return page, err
//	})
//	for invoice, err := range pages { … }
func Paginate[T any, P CursorPage[T]](fetch func(cursor string) (P, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		cursor := ""
		for {
			page, err := fetch(cursor)
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			for _, item := range page.GetData() {
				if !yield(item, nil) {
					return
				}
			}
			if !page.GetHasMore() || page.GetNextCursor() == "" {
				return
			}
			cursor = page.GetNextCursor()
		}
	}
}
