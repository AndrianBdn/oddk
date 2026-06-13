package operations

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildCreateDatabaseSQL(t *testing.T) {
	cases := []struct {
		name      string
		target    string
		meta      DatabaseMeta
		withOwner bool
		want      string
	}{
		{
			name:      "with owner",
			target:    "appdb",
			meta:      DatabaseMeta{Name: "appdb", Owner: "appowner", Encoding: "UTF8", Collate: "C", Ctype: "C", LocProvider: "c"},
			withOwner: true,
			want:      `CREATE DATABASE "appdb" OWNER = "appowner" TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'`,
		},
		{
			name:      "without owner",
			target:    "appdb",
			meta:      DatabaseMeta{Name: "appdb", Owner: "appowner", Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8", LocProvider: "c"},
			withOwner: false,
			want:      `CREATE DATABASE "appdb" TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'en_US.utf8' LC_CTYPE 'en_US.utf8'`,
		},
		{
			name:      "rename target differs from source name",
			target:    "appdb_restored",
			meta:      DatabaseMeta{Name: "appdb", Owner: "o", Encoding: "UTF8", Collate: "C", Ctype: "C", LocProvider: "c"},
			withOwner: false,
			want:      `CREATE DATABASE "appdb_restored" TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'`,
		},
		{
			name:      "quotes embedded special characters safely",
			target:    `we"ird`,
			meta:      DatabaseMeta{Name: `we"ird`, Owner: `ro'le`, Encoding: "UTF8", Collate: "C", Ctype: "C", LocProvider: "c"},
			withOwner: true,
			want:      `CREATE DATABASE "we""ird" OWNER = "ro'le" TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildCreateDatabaseSQL(c.target, c.meta, c.withOwner)
			if got != c.want {
				t.Errorf("buildCreateDatabaseSQL:\n got: %s\nwant: %s", got, c.want)
			}
		})
	}
}

func TestReadDatabaseMetadataRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := []DatabaseMeta{
		{Name: "appdb", Owner: "appowner", Encoding: "UTF8", Collate: "C", Ctype: "C", LocProvider: "c"},
		{Name: "postgres", Owner: "postgres", Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8", LocProvider: "c"},
	}
	if err := writeDatabaseMetadata(dir, want); err != nil {
		t.Fatalf("writeDatabaseMetadata: %v", err)
	}

	got, found, err := readDatabaseMetadata(dir)
	if err != nil {
		t.Fatalf("readDatabaseMetadata: %v", err)
	}
	if !found {
		t.Fatal("expected found=true after writing databases.json")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestReadDatabaseMetadataAbsent(t *testing.T) {
	dir := t.TempDir() // empty: no databases.json (simulates an older backup archive)
	metas, found, err := readDatabaseMetadata(dir)
	if err != nil {
		t.Fatalf("expected nil error for absent file, got: %v", err)
	}
	if found {
		t.Errorf("expected found=false for absent file at %s", filepath.Join(dir, databaseMetadataFile))
	}
	if metas != nil {
		t.Errorf("expected nil metas for absent file, got: %+v", metas)
	}
}
