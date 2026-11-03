package config

import "testing"

func TestMetaTableName(t *testing.T) {
	for name, ok := range map[string]bool{"x_asset_meta": true, "seapin_shadow.x_asset_meta": true,
		"x; drop table x": false, `a"b`: false, "A.b": false, "a.b.c": false, "": false} {
		if metaTableRe.MatchString(name) != ok {
			t.Errorf("%q: want ok=%v", name, ok)
		}
	}
}
