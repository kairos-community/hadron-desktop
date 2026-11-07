package main

import (
	"context"

	"github.com/godbus/dbus/v5"
)

func installedDict(x InstalledExt) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"name":    dbus.MakeVariant(x.Name),
		"file":    dbus.MakeVariant(x.File),
		"enabled": dbus.MakeVariant(x.Enabled),
		"merged":  dbus.MakeVariant(x.Merged),
		"source":  dbus.MakeVariant(x.Source),
	}
}

func catalogDict(e CatalogEntry) map[string]dbus.Variant {
	versions := e.Versions
	if versions == nil {
		versions = []string{}
	}
	return map[string]dbus.Variant{
		"name":        dbus.MakeVariant(e.Name),
		"title":       dbus.MakeVariant(e.Title),
		"description": dbus.MakeVariant(e.Description),
		"latest":      dbus.MakeVariant(e.Latest),
		"catalog":     dbus.MakeVariant(e.Catalog),
		"versions":    dbus.MakeVariant(versions),
		"installed":   dbus.MakeVariant(e.Installed),
		"enabled":     dbus.MakeVariant(e.Enabled),
	}
}

// Extensions is open to everyone, like Status: it reads a directory.
func (a busAPI) Extensions() ([]map[string]dbus.Variant, *dbus.Error) {
	done, derr := a.s.enter()
	if derr != nil {
		return nil, derr
	}
	defer done()
	list, err := a.s.d.Extensions()
	if err != nil {
		return nil, busError(err)
	}
	out := make([]map[string]dbus.Variant, 0, len(list))
	for _, x := range list {
		out = append(out, installedDict(x))
	}
	return out, nil
}

// Catalog is open to everyone, like Releases, and shares its network slot.
func (a busAPI) Catalog() ([]map[string]dbus.Variant, *dbus.Error) {
	done, derr := a.s.enter()
	if derr != nil {
		return nil, derr
	}
	defer done()
	free, derr := a.s.network()
	if derr != nil {
		return nil, derr
	}
	defer free()
	entries, err := a.s.d.Catalog(context.Background())
	if err != nil {
		return nil, busError(err)
	}
	out := make([]map[string]dbus.Variant, 0, len(entries))
	for _, e := range entries {
		out = append(out, catalogDict(e))
	}
	return out, nil
}

// ExtensionFiles reads an installed image with systemd-dissect. It changes
// nothing, so it needs no authorization, but it mounts an image as root, so
// it takes the one slot the network calls share: no caller can start many.
func (a busAPI) ExtensionFiles(name string) ([]string, *dbus.Error) {
	done, derr := a.s.enter()
	if derr != nil {
		return nil, derr
	}
	defer done()
	if !extName.MatchString(name) {
		return nil, busError(errNotExtName(name))
	}
	free, derr := a.s.network()
	if derr != nil {
		return nil, derr
	}
	defer free()
	files, err := a.s.d.ExtensionFiles(name)
	if err != nil {
		return nil, busError(err)
	}
	if files == nil {
		files = []string{}
	}
	return files, nil
}

func (a busAPI) InstallExtension(sender dbus.Sender, ref, version string, force bool) *dbus.Error {
	done, derr := a.s.enter()
	if derr != nil {
		return derr
	}
	defer done()
	if err := a.s.authorize(sender, actionExtensions); err != nil {
		return err
	}
	return busError(a.s.d.InstallExtension(ref, version, force))
}

func (a busAPI) SetExtensionEnabled(sender dbus.Sender, name string, on, force bool) *dbus.Error {
	done, derr := a.s.enter()
	if derr != nil {
		return derr
	}
	defer done()
	if err := a.s.authorize(sender, actionExtensions); err != nil {
		return err
	}
	return busError(a.s.d.SetExtensionEnabled(name, on, force))
}

func (a busAPI) RemoveExtension(sender dbus.Sender, name string) *dbus.Error {
	done, derr := a.s.enter()
	if derr != nil {
		return derr
	}
	defer done()
	if err := a.s.authorize(sender, actionExtensions); err != nil {
		return err
	}
	return busError(a.s.d.RemoveExtension(name))
}
