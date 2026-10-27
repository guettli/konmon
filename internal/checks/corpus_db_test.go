package checks

import (
	"database/sql"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	_ "modernc.org/sqlite"
)

func readCorpusDB(t *testing.T, path string) []Item {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(t.Context(), `SELECT api_group, api_version, resource, object FROM objects WHERE deleted_at IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var items []Item
	for rows.Next() {
		var gvr schema.GroupVersionResource
		var data []byte
		if err := rows.Scan(&gvr.Group, &gvr.Version, &gvr.Resource, &data); err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			t.Fatal(err)
		}
		items = append(items, Item{GVR: gvr, Obj: &unstructured.Unstructured{Object: normalize(obj).(map[string]any)}})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return items
}
