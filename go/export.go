package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func exportRun(rundir, out string) {
	if rundir == "" {
		link := filepath.Join(Root, "runs", "latest")
		b, err := os.Readlink(link)
		if err != nil {
			// windows symlink 可能不可读: 用 latest.port 里的路径
			if pb, e2 := os.ReadFile(filepath.Join(Root, "runs", "latest.port")); e2 == nil {
				fs := strings.Fields(string(pb))
				if len(fs) > 1 {
					b = fs[1]
					err = nil
				}
			}
			if err != nil {
				fmt.Println("no run dir")
				return
			}
		}
		rundir = string(b)
	}
	if out == "" {
		out = filepath.Base(rundir) + ".zip"
	}
	zf, err := os.Create(out)
	if err != nil {
		fmt.Println(err)
		return
	}
	zw := zip.NewWriter(zf)
	filepath.Walk(rundir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(rundir, p)
		w, err := zw.Create(rel)
		if err != nil {
			return nil
		}
		f, _ := os.Open(p)
		defer f.Close()
		io.Copy(w, f)
		return nil
	})
	zw.Close()
	zf.Close()
	fmt.Println("exported → " + out)
}

func importRun(zippath string) {
	zr, err := zip.OpenReader(zippath)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer zr.Close()
	base := strings.TrimSuffix(filepath.Base(zippath), ".zip")
	dst := filepath.Join(Root, "runs", "imported-"+base)
	for _, f := range zr.File {
		fp := filepath.Join(dst, f.Name)
		os.MkdirAll(filepath.Dir(fp), 0o755)
		rc, err := f.Open()
		if err != nil {
			continue
		}
		w, _ := os.Create(fp)
		io.Copy(w, rc)
		w.Close()
		rc.Close()
	}
	fmt.Println("imported → " + dst)
}
