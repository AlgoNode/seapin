// coverage measures how much of what x_asset_meta lists each seapin region
// holds: the M6 gate (PLAN.md: both regions hold >= 99% of the objects
// x_asset_meta lists before the old gateway's caches are retired).
//
// Inputs (all read-only exports):
//
//	x_asset_meta extract: TSV(.gz) with columns asset, class, cid[/path], urlonly, mimetype, bytes, ...
//	                      (seapin-m0/pg_extract.sql)
//	per region directory: objects.tsv.zst (cid, status, reason, size),
//	                      aliases.tsv.zst (root, subpath, leaf),
//	                      fetches.tsv.zst (root, subpath, status, attempts, leaf, last_error)
//
// usage: go run ./scripts/coverage <x_asset_meta.tsv.gz> <outdir> <name>=<regiondir>...
//
// Each distinct (CID, path) is resolved as seapin does: the fetch row's leaf,
// else the alias, else the CID itself when there is no path; then the stored
// object's status. Objects seapin has never been asked for are "unknown".
package main

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"seapin/internal/cidkey"
)

type want struct {
	class   string
	bytes   int64  // from x_asset_meta; -1 unknown
	asset   uint64 // one asset listing it
	urlKind string // that asset's URL form (M0 extract: ipfs, arc19, path, subdomain, http, none, null)
}

type fetch struct{ status, leaf, err string }

type object struct {
	status, reason string
	size           int64
}

type region struct {
	name    string
	fetches map[string]fetch
	aliases map[string]string
	objects map[string]object
	refs    map[uint64][]string // asset -> its refs' "root\tsubpath" (optional refs.tsv.zst)
}

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: coverage <x_asset_meta.tsv.gz> <outdir> <name>=<regiondir>...")
		os.Exit(2)
	}
	wants, invalid, rows := loadWants(os.Args[1])
	out := os.Args[2]
	must(os.MkdirAll(out, 0o755))
	var regions []*region
	for _, a := range os.Args[3:] {
		name, dir, _ := strings.Cut(a, "=")
		regions = append(regions, loadRegion(name, dir))
	}
	fmt.Printf("x_asset_meta: %d rows with a CID, %d distinct (CID, path) objects, %d unparseable CIDs\n\n",
		rows, len(wants), invalid)

	keys := make([]string, 0, len(wants))
	for k := range wants {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	both := 0
	var bothBytes int64
	perRegion := map[string]map[string]string{}
	for _, r := range regions {
		cats := map[string]string{}
		for _, k := range keys {
			cats[k] = r.resolve(k)
			if cats[k] == "unknown to seapin" && r.refs != nil {
				cats[k] = "unknown: " + r.why(wants[k])
			}
		}
		perRegion[r.name] = cats
		report(r.name, keys, wants, cats)
		dumpMisses(filepath.Join(out, r.name+"-misses.tsv"), keys, wants, cats, r)
	}
	for _, k := range keys {
		ok := true
		for _, r := range regions {
			if perRegion[r.name][k] != "cached" {
				ok = false
			}
		}
		if ok {
			both++
			if b := wants[k].bytes; b > 0 {
				bothBytes += b
			}
		}
	}
	fmt.Printf("cached in every region: %d of %d (%.2f%%), %.1f GB by x_asset_meta sizes\n",
		both, len(keys), 100*float64(both)/float64(len(keys)), float64(bothBytes)/1e9)
}

func (r *region) resolve(k string) string {
	root, sub, _ := strings.Cut(k, "\t")
	f, hasFetch := r.fetches[k]
	leaf := f.leaf
	if leaf == "" {
		leaf = r.aliases[k]
	}
	if leaf == "" && sub == "" {
		leaf = root
	}
	if o, ok := r.objects[leaf]; ok && leaf != "" {
		switch o.status {
		case "blocked":
			reason, _, _ := strings.Cut(o.reason, ":")
			return "blocked:" + reason
		default:
			return o.status
		}
	}
	if hasFetch {
		return "fetch " + f.status
	}
	return "unknown to seapin"
}

// why splits an unknown object by what seapin has for the asset listing it.
func (r *region) why(w want) string {
	if r.refs == nil {
		return ""
	}
	if len(r.refs[w.asset]) == 0 {
		return "asset has no seapin ref"
	}
	return "asset ref points elsewhere"
}

type agg struct {
	n     int
	bytes int64
}

func report(name string, keys []string, wants map[string]want, cats map[string]string) {
	total := map[string]*agg{}
	byClass := map[string]map[string]*agg{}
	for _, k := range keys {
		w, c := wants[k], cats[k]
		for _, m := range []map[string]*agg{total, sub(byClass, w.class)} {
			a := m[c]
			if a == nil {
				a = &agg{}
				m[c] = a
			}
			a.n++
			if w.bytes > 0 {
				a.bytes += w.bytes
			}
		}
	}
	fmt.Printf("== %s\n%-26s %10s %8s %10s\n", name, "outcome", "objects", "%", "GB")
	var cats_ []string
	for c := range total {
		cats_ = append(cats_, c)
	}
	sort.Slice(cats_, func(i, j int) bool { return total[cats_[i]].n > total[cats_[j]].n })
	for _, c := range cats_ {
		a := total[c]
		fmt.Printf("%-26s %10d %7.2f%% %10.1f\n", c, a.n, 100*float64(a.n)/float64(len(keys)), float64(a.bytes)/1e9)
	}
	var classes []string
	for c := range byClass {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	fmt.Printf("by class (cached / all):")
	for _, cl := range classes {
		n, c := 0, 0
		for cat, a := range byClass[cl] {
			n += a.n
			if cat == "cached" {
				c += a.n
			}
		}
		fmt.Printf("  %s %d/%d (%.2f%%)", label(cl), c, n, 100*float64(c)/float64(n))
	}
	fmt.Print("\n")
	kinds := map[string]map[string]int{}
	for _, k := range keys {
		c := cats[k]
		if c == "cached" {
			continue
		}
		if kinds[c] == nil {
			kinds[c] = map[string]int{}
		}
		kinds[c][label(wants[k].urlKind)+"/"+label(wants[k].class)]++
	}
	for _, c := range cats_ {
		if kinds[c] == nil {
			continue
		}
		type kv struct {
			k string
			n int
		}
		var l []kv
		for k, n := range kinds[c] {
			l = append(l, kv{k, n})
		}
		sort.Slice(l, func(i, j int) bool { return l[i].n > l[j].n })
		fmt.Printf("  %s by url kind/class:", c)
		for i, e := range l {
			if i == 8 {
				break
			}
			fmt.Printf(" %s %d", e.k, e.n)
		}
		fmt.Println()
	}
	fmt.Println()
}

func sub(m map[string]map[string]*agg, k string) map[string]*agg {
	if m[k] == nil {
		m[k] = map[string]*agg{}
	}
	return m[k]
}

func label(c string) string {
	if c == "" {
		return "(none)"
	}
	return c
}

func dumpMisses(path string, keys []string, wants map[string]want, cats map[string]string, r *region) {
	f, err := os.Create(path)
	must(err)
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	fmt.Fprintln(w, "outcome\tclass\tbytes\tcid\tpath\tlast_error\tasset\turl_kind")
	for _, k := range keys {
		c := cats[k]
		if c == "cached" {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%d\t%s\n", c, wants[k].class, wants[k].bytes, k, r.fetches[k].err, wants[k].asset, wants[k].urlKind)
	}
}

func loadWants(path string) (map[string]want, int, int) {
	f, err := os.Open(path)
	must(err)
	defer f.Close()
	var rd io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		g, err := gzip.NewReader(f)
		must(err)
		rd = g
	}
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	wants := map[string]want{}
	invalid, rows := 0, 0
	for sc.Scan() {
		col := strings.Split(sc.Text(), "\t")
		if len(col) < 6 || col[2] == `\N` || col[2] == "" {
			continue
		}
		rows++
		root, p, _ := strings.Cut(col[2], "/")
		norm, err := cidkey.Normalize(root)
		if err != nil {
			invalid++
			continue
		}
		// x_asset_meta stores paths URL-encoded ("Flamingo%2357.png"); seapin keeps the name ("Flamingo#57.png")
		if u, err := url.PathUnescape(p); err == nil {
			p = u
		}
		clean, err := cidkey.CleanSubpath(p)
		if err != nil {
			invalid++
			continue
		}
		k := norm + "\t" + clean
		b := int64(-1)
		if col[5] != `\N` {
			b, _ = strconv.ParseInt(col[5], 10, 64)
		}
		asset, _ := strconv.ParseUint(col[0], 10, 64)
		kind := ""
		if len(col) > 10 {
			kind = col[10]
		}
		if w, ok := wants[k]; !ok {
			wants[k] = want{class: col[1], bytes: b, asset: asset, urlKind: kind}
		} else if b > w.bytes {
			w.bytes = b
			wants[k] = w
		}
	}
	must(sc.Err())
	return wants, invalid, rows
}

func loadRegion(name, dir string) *region {
	r := &region{name: name, fetches: map[string]fetch{}, aliases: map[string]string{}, objects: map[string]object{}}
	eachRow(filepath.Join(dir, "fetches.tsv.zst"), func(c []string) {
		r.fetches[c[0]+"\t"+c[1]] = fetch{status: c[2], leaf: c[4], err: c[5]}
	})
	eachRow(filepath.Join(dir, "aliases.tsv.zst"), func(c []string) { r.aliases[c[0]+"\t"+c[1]] = c[2] })
	eachRow(filepath.Join(dir, "objects.tsv.zst"), func(c []string) {
		size, _ := strconv.ParseInt(c[3], 10, 64)
		r.objects[c[0]] = object{status: c[1], reason: c[2], size: size}
	})
	if _, err := os.Stat(filepath.Join(dir, "refs.tsv.zst")); err == nil {
		r.refs = map[uint64][]string{}
		eachRow(filepath.Join(dir, "refs.tsv.zst"), func(c []string) { // asset, path, root, subpath, round
			a, _ := strconv.ParseUint(c[0], 10, 64)
			r.refs[a] = append(r.refs[a], c[2]+"\t"+c[3])
		})
	}
	return r
}

func eachRow(path string, fn func([]string)) {
	cmd := exec.Command("zstd", "-dc", path)
	out, err := cmd.StdoutPipe()
	must(err)
	must(cmd.Start())
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		fn(strings.Split(sc.Text(), "\t"))
	}
	must(sc.Err())
	must(cmd.Wait())
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
