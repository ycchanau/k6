package js

import (
	"fmt"
	"net/url"

	"github.com/dop251/goja"
	"go.k6.io/k6/js/compiler"
	"go.k6.io/k6/js/modules"
)

// cjsModule represents a commonJS module
type cjsModule struct {
	prg *goja.Program
	url *url.URL
}

var _ module = &cjsModule{}

type cjsModuleInstance struct {
	mod       *cjsModule
	moduleObj *goja.Object
	vu        modules.VU
}

func (c *cjsModule) Instantiate(vu modules.VU) moduleInstance {
	return &cjsModuleInstance{vu: vu, mod: c}
}

func (c *cjsModuleInstance) execute() error {
	rt := c.vu.Runtime()
	exports := rt.NewObject()
	c.moduleObj = rt.NewObject()
	err := c.moduleObj.Set("exports", exports)
	if err != nil {
		return fmt.Errorf("error while getting ready to import commonJS, couldn't set exports property of module: %w",
			err)
	}

	// Run the program.
	f, err := rt.RunProgram(c.mod.prg)
	if err != nil {
		return err
	}
	if call, ok := goja.AssertFunction(f); ok {
		if _, err = call(exports, c.moduleObj, exports); err != nil {
			return err
		}
	}

	return nil
}

func (c *cjsModuleInstance) exports() *goja.Object {
	exportsV := c.moduleObj.Get("exports")
	if goja.IsNull(exportsV) || goja.IsUndefined(exportsV) {
		return nil
	}
	return exportsV.ToObject(c.vu.Runtime())
}

type cjsModuleLoader func(specifier *url.URL, name string) (*cjsModule, error)

func cjsmoduleFromString(fileURL *url.URL, data []byte, c *compiler.Compiler) (*cjsModule, error) {
	pgm, _, err := c.Compile(string(data), fileURL.String(), false)
	if err != nil {
		return nil, err
	}
	return &cjsModule{prg: pgm, url: fileURL}, nil
}
