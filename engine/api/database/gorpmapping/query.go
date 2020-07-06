package gorpmapping

import (
	"fmt"
	"strings"

	"github.com/ovh/cds/engine/gorpmapper"
)

type Query struct {
	gorpmapper.Query
}

// NewQuery returns a new query from given string request.
func NewQuery(q string) Query { return Query{gorpmapper.NewQuery(q)} }

// Args store query arguments.
func (q Query) Args(as ...interface{}) Query {
	q.Query.Arguments = as
	return q
}

func (q Query) Limit(i int) Query {
	q.Query.Limit(i)
	return q
}

// IDsToQueryString returns a comma separated list of given ids.
func IDsToQueryString(ids []int64) string {
	res := make([]string, len(ids))
	for i := range ids {
		res[i] = fmt.Sprintf("%d", ids[i])
	}
	return strings.Join(res, ",")
}

// IDStringsToQueryString returns a comma separated list of given string ids.
func IDStringsToQueryString(ids []string) string {
	return strings.Join(ids, ",")
}
