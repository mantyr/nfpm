// Package rpm implements nfpm.Packager providing .rpm bindings through rpmbuild.
package rpm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/goreleaser/nfpm"
	"github.com/pkg/errors"
)

func init() {
	nfpm.Register("rpm", Default)
}

// Default deb packager
var Default = &RPM{}

// RPM is a RPM packager implementation
type RPM struct{}

// Package writes a new RPM package to the given writer using the given info
func (*RPM) Package(info nfpm.Info, w io.Writer) error {
	if info.Arch == "amd64" {
		info.Arch = "x86_64"
	}
	_, err := exec.LookPath("rpmbuild")
	if err != nil {
		return fmt.Errorf("rpmbuild not present in $PATH")
	}
	temps, err := setupTempFiles(info)
	if err != nil {
		return err
	}
	if err = createTarGz(info, temps.Folder, temps.Source); err != nil {
		return errors.Wrap(err, "failed to create tar.gz")
	}
	if err = createSpec(info, temps.Spec); err != nil {
		return errors.Wrap(err, "failed to create rpm spec file")
	}

	var args = []string{
		"--define", fmt.Sprintf("_topdir %s", temps.Root),
		"--define", fmt.Sprintf("_tmppath %s/tmp", temps.Root),
		"--target", fmt.Sprintf("%s-unknown-%s", info.Arch, info.Platform),
		"-ba",
		"SPECS/" + info.Name + ".spec",
	}
	// #nosec
	cmd := exec.Command("rpmbuild", args...)
	cmd.Dir = temps.Root
	out, err := cmd.CombinedOutput()
	if err != nil {
		var msg = "rpmbuild failed"
		if string(out) != "" {
			msg += ": " + string(out)
		}
		return errors.Wrap(err, msg)
	}

	rpm, err := os.Open(temps.RPM)
	if err != nil {
		return errors.Wrap(err, "failed open rpm file")
	}
	_, err = io.Copy(w, rpm)
	return errors.Wrap(err, "failed to copy rpm file to writer")
}

type rpmbuildVersion struct {
	Major, Minor, Path int
}

func getRpmbuildVersion() (rpmbuildVersion, error) {
	// #nosec
	bts, err := exec.Command("rpmbuild", "--version").CombinedOutput()
	if err != nil {
		return rpmbuildVersion{}, errors.Wrap(err, "failed to get rpmbuild version")
	}
	var v = make([]int, 3)
	vs := strings.TrimSuffix(strings.TrimPrefix(string(bts), "RPM version "), "\n")
	for i, part := range strings.Split(vs, ".")[:3] {
		pi, err := strconv.Atoi(part)
		if err != nil {
			return rpmbuildVersion{}, errors.Wrapf(err, "could not parse version %s", vs)
		}
		v[i] = pi
	}
	return rpmbuildVersion{
		Major: v[0],
		Minor: v[1],
		Path:  v[2],
	}, nil
}

func createSpec(info nfpm.Info, path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		return errors.Wrap(err, "failed to create spec")
	}
	vs, err := getRpmbuildVersion()
	if err != nil {
		return err
	}
	return writeSpec(file, info, vs)
}

func writeSpec(w io.Writer, info nfpm.Info, vs rpmbuildVersion) error {
	var tmpl = template.New("spec")
	tmpl.Funcs(template.FuncMap{
		"first_line": func(str string) string {
			return strings.Split(str, "\n")[0]
		},
	})
	type data struct {
		Info   nfpm.Info
		RPM413 bool
	}
	if err := template.Must(tmpl.Parse(specTemplate)).Execute(w, data{
		Info:   info,
		RPM413: vs.Major >= 4 && vs.Minor >= 13,
	}); err != nil {
		return errors.Wrap(err, "failed to parse spec template")
	}
	return nil
}

type tempFiles struct {
	// Root folder - topdir on rpm's slang
	Root string
	// Folder is the name of subfolders and etc, in the `name-version` format
	Folder string
	// Source is the path the .tar.gz file should be in
	Source string
	// Spec is the path the .spec file should be in
	Spec string
	// RPM is the path where the .rpm file should be generated
	RPM string
}

func setupTempFiles(info nfpm.Info) (tempFiles, error) {
	root, err := ioutil.TempDir("", info.Name)
	if err != nil {
		return tempFiles{}, errors.Wrap(err, "failed to create temp dir")
	}
	if err := createDirs(root); err != nil {
		return tempFiles{}, errors.Wrap(err, "failed to rpm dir structure")
	}
	folder := fmt.Sprintf("%s-%s", info.Name, info.Version)
	return tempFiles{
		Root:   root,
		Folder: folder,
		Source: filepath.Join(root, "SOURCES", folder+".tar.gz"),
		Spec:   filepath.Join(root, "SPECS", info.Name+".spec"),
		RPM:    filepath.Join(root, "RPMS", info.Arch, fmt.Sprintf("%s-1.%s.rpm", folder, info.Arch)),
	}, nil
}

func createDirs(root string) error {
	for _, folder := range []string{
		"RPMS",
		"SRPMS",
		"BUILD",
		"SOURCES",
		"SPECS",
		"tmp",
	} {
		path := filepath.Join(root, folder)
		if err := os.Mkdir(path, 0700); err != nil {
			return errors.Wrapf(err, "failed to create %s", path)
		}
	}
	return nil
}

func createTarGz(info nfpm.Info, root, file string) error {
	var buf bytes.Buffer
	var compress = gzip.NewWriter(&buf)
	var out = tar.NewWriter(compress)
	// the writers are properly closed later, this is just in case that we have
	// an error in another part of the code.
	defer out.Close()      // nolint: errcheck
	defer compress.Close() // nolint: errcheck

	for _, files := range []map[string]string{info.Files, info.ConfigFiles} {
		for src, dst := range files {
			if err := copyToTarGz(out, root, src, dst); err != nil {
				return err
			}
		}
	}
	if err := out.Close(); err != nil {
		return errors.Wrap(err, "failed to close data.tar.gz writer")
	}
	if err := compress.Close(); err != nil {
		return errors.Wrap(err, "failed to close data.tar.gz gzip writer")
	}
	if err := ioutil.WriteFile(file, buf.Bytes(), 0666); err != nil {
		return errors.Wrap(err, "could not write to .tar.gz file")
	}
	return nil
}

func copyToTarGz(out *tar.Writer, root, src, dst string) error {
	file, err := os.OpenFile(src, os.O_RDONLY, 0600)
	if err != nil {
		return errors.Wrap(err, "could not add file to the archive")
	}
	// don't really care if Close() errs
	defer file.Close() // nolint: errcheck
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	var header = tar.Header{
		Name:    filepath.Join(root, dst),
		Size:    info.Size(),
		Mode:    int64(info.Mode()),
		ModTime: info.ModTime(),
	}
	if err := out.WriteHeader(&header); err != nil {
		return errors.Wrapf(err, "cannot write header of %s to data.tar.gz", header.Name)
	}
	if _, err := io.Copy(out, file); err != nil {
		return errors.Wrapf(err, "cannot write %s to data.tar.gz", header.Name)
	}
	return nil
}

const specTemplate = `
%define __spec_install_post %{nil}
%define debug_package %{nil}
%define __os_install_post %{_dbpath}/brp-compress
%define _arch {{ .Info.Arch }}
%define _bindir {{ .Info.Bindir }}

Name: {{ .Info.Name }}
Summary: {{ first_line .Info.Description }}
Version: {{ .Info.Version }}
Release: 1
License: {{ .Info.License }}
Group: Development/Tools
SOURCE0 : %{name}-%{version}.tar.gz
URL: {{ .Info.Homepage }}
BuildRoot: %{_tmppath}/%{name}-%{version}-%{release}-root

{{ range $index, $element := .Info.Replaces }}
Obsoletes: {{ . }}
{{ end }}

{{ range $index, $element := .Info.Conflicts }}
Conflicts: {{ . }}
{{ end }}

{{ range $index, $element := .Info.Provides }}
Provides: {{ . }}
{{ end }}

{{ range $index, $element := .Info.Depends }}
Requires: {{ . }}
{{ end }}

{{ if .RPM413 }}
{{ range $index, $element := .Info.Recommends }}
Recommends: {{ . }}
{{ end }}

{{ range $index, $element := .Info.Suggests }}
Suggests: {{ . }}
{{ end }}
{{ end }}

%description
{{ .Info.Description }}

%prep
%setup -q

%build
# Empty section.

%install
rm -rf %{buildroot}
mkdir -p  %{buildroot}

# in builddir
cp -a * %{buildroot}

%clean
rm -rf %{buildroot}

%files
%defattr(-,root,root,-)
{{ range $index, $element := .Info.Files }}
{{ . }}
{{ end }}
%{_bindir}/*
{{ range $index, $element := .Info.ConfigFiles }}
{{ . }}
{{ end }}
{{ range $index, $element := .Info.ConfigFiles }}
%config(noreplace) {{ . }}
{{ end }}

%changelog
# noop
`
