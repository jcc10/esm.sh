package server

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/ije/gox/utils"
)

type ESMBuild struct {
	NamedExports     []string `json:"-"`
	HasExportDefault bool     `json:"d"`
	CJS              bool     `json:"c"`
	Dts              string   `json:"t"`
	TypesOnly        bool     `json:"o"`
	PackageCSS       bool     `json:"s"`
}

type BuildTask struct {
	BuildArgs

	Pkg          Pkg
	CdnOrigin    string
	Target       string
	BuildVersion int
	Dev          bool
	Bundle       bool

	// internal
	id    string
	wd    string
	stage string
}

func (task *BuildTask) Build() (esm *ESMBuild, err error) {
	if task.wd == "" {
		task.wd = path.Join(cfg.WorkDir, fmt.Sprintf("npm/%s@%s", task.Pkg.Name, task.Pkg.Version))
		ensureDir(task.wd)

		if err != nil {
			return
		}

		if cfg.NpmToken != "" {
			rcFilePath := path.Join(task.wd, ".npmrc")
			if !fileExists(rcFilePath) {
				err = os.WriteFile(rcFilePath, []byte("_authToken=${ESM_NPM_TOKEN}"), 0644)
				if err != nil {
					log.Errorf("Failed to create .npmrc file: %v", err)
					return
				}
			}
		}
	}

	// TODO: Remove node_modules of the idle working dir
	// 	defer func() {
	// 		err := os.RemoveAll(task.wd)
	// 		if err != nil {
	// 			log.Warnf("clean build(%s) dir: %v", task.ID(), err)
	// 		}
	// 	}()

	task.stage = "install"
	err = installPackage(task.wd, task.Pkg)
	if err != nil {
		return
	}

	return task.build()
}

func (task *BuildTask) build() (esm *ESMBuild, err error) {
	if task.Target == "raw" {
		return
	}

	task.stage = "init"
	esm, npm, err := task.init()
	if err != nil {
		return
	}

	if task.Target == "types" {
		if npm.Types != "" {
			dts := npm.Name + "@" + npm.Version + path.Join("/", npm.Types)
			task.stage = "transform-dts"
			task.buildDTS(dts)
		}
		return
	}

	if isTypesOnlyPackage(npm) {
		dts := npm.Name + "@" + npm.Version + path.Join("/", npm.Types)
		task.stage = "transform-dts"
		task.buildDTS(dts)
		task.storeToDB(esm)
		return
	}

	task.stage = "build"
	defer func() {
		if err != nil {
			esm = nil
		}
	}()

	var entryPoint string
	var input *api.StdinOptions

	if npm.Module == "" {
		buf := bytes.NewBuffer(nil)
		importPath := task.Pkg.ImportPath()
		fmt.Fprintf(buf, `import * as __module from "%s";`, importPath)
		if len(esm.NamedExports) > 0 {
			var exports []string
			for _, k := range esm.NamedExports {
				if k == "__esModule" {
					fmt.Fprintf(buf, "export const __esModule = true;")
				} else {
					exports = append(exports, k)
				}
			}
			if len(exports) > 0 {
				fmt.Fprintf(buf, `export const { %s } = __module;`, strings.Join(exports, ","))
			}
		}
		fmt.Fprintf(buf, "const { default: __default, ...__rest } = __module;")
		fmt.Fprintf(buf, "export default (__default !== undefined ? __default : __rest);")
		// Default reexport all members from original module to prevent missing named exports members
		fmt.Fprintf(buf, `export * from "%s";`, importPath)
		input = &api.StdinOptions{
			Contents:   buf.String(),
			ResolveDir: task.wd,
			Sourcefile: "index.js",
		}
	} else {
		if task.treeShaking.Size() > 0 {
			buf := bytes.NewBuffer(nil)
			importPath := task.Pkg.ImportPath()
			fmt.Fprintf(buf, `export { %s } from "%s";`, strings.Join(task.treeShaking.Values(), ","), importPath)
			input = &api.StdinOptions{
				Contents:   buf.String(),
				ResolveDir: task.wd,
				Sourcefile: "index.js",
			}
		} else {
			entryPoint = path.Join(task.wd, "node_modules", npm.Name, npm.Module)
		}
	}

	nodeEnv := "production"
	if task.Dev {
		nodeEnv = "development"
	}
	define := map[string]string{
		"__filename":                  fmt.Sprintf(`"%s%s/%s"`, task.CdnOrigin, cfg.BasePath, task.ID()),
		"__dirname":                   fmt.Sprintf(`"%s%s/%s"`, task.CdnOrigin, cfg.BasePath, path.Dir(task.ID())),
		"Buffer":                      "__Buffer$",
		"process":                     "__Process$",
		"setImmediate":                "__setImmediate$",
		"clearImmediate":              "clearTimeout",
		"require.resolve":             "__rResolve$",
		"process.env.NODE_ENV":        fmt.Sprintf(`"%s"`, nodeEnv),
		"global":                      "__global$",
		"global.Buffer":               "__Buffer$",
		"global.process":              "__Process$",
		"global.setImmediate":         "__setImmediate$",
		"global.clearImmediate":       "clearTimeout",
		"global.require.resolve":      "__rResolve$",
		"global.process.env.NODE_ENV": fmt.Sprintf(`"%s"`, nodeEnv),
	}
	externalDeps := newStringSet()
	implicitExternal := newStringSet()
	browserExclude := map[string]*stringSet{}

rebuild:
	options := api.BuildOptions{
		Outdir:            "/esbuild",
		Write:             false,
		Bundle:            true,
		Conditions:        task.conditions.Values(),
		Target:            targets[task.Target],
		Format:            api.FormatESModule,
		Platform:          api.PlatformBrowser,
		MinifyWhitespace:  !task.Dev,
		MinifyIdentifiers: !task.Dev,
		MinifySyntax:      !task.Dev,
		KeepNames:         task.keepNames,         // prevent class/function names erasing
		IgnoreAnnotations: task.ignoreAnnotations, // some libs maybe use wrong side-effect annotations
		PreserveSymlinks:  true,
		Plugins: []api.Plugin{{
			Name: "esm",
			Setup: func(build api.PluginBuild) {
				build.OnResolve(
					api.OnResolveOptions{Filter: ".*"},
					func(args api.OnResolveArgs) (api.OnResolveResult, error) {
						if strings.HasPrefix(args.Path, "data:") {
							return api.OnResolveResult{External: true}, nil
						}

						for _, name := range reservedPackages {
							if args.Path == name || strings.HasPrefix(args.Path, name+"/") {
								if task.Target == "deno" || task.Target == "denonext" {
									pkgName, submodule := splitPkgPath(args.Path)
									version := "latest"
									if pkgName == task.Pkg.Name {
										version = task.Pkg.Version
									} else if v, ok := npm.Dependencies[pkgName]; ok {
										version = v
									} else if v, ok := npm.PeerDependencies[pkgName]; ok {
										version = v
									}
									p, _, err := getPackageInfo(task.wd, pkgName, version)
									if err == nil {
										pkg := Pkg{
											Name:      p.Name,
											Version:   p.Version,
											Submodule: submodule,
										}
										return api.OnResolveResult{Path: fmt.Sprintf("npm:%s", pkg.String()), External: true}, nil
									}
								}
								return api.OnResolveResult{Path: fmt.Sprintf(
									"%s/error.js?type=unsupported-npm-package&name=%s&importer=%s",
									cfg.BasePath,
									args.Path,
									task.Pkg.Name,
								), External: true}, nil
							}
						}

						// ignore `require()` expression
						if task.ignoreRequire && (args.Kind == api.ResolveJSRequireCall || args.Kind == api.ResolveJSRequireResolve) && npm.Module != "" {
							return api.OnResolveResult{Path: args.Path, External: true}, nil
						}

						// strip the tailing slash
						specifier := strings.TrimSuffix(args.Path, "/")
						// strip the `node:` prefix
						specifier = strings.TrimPrefix(specifier, "node:")

						// use `browser` field of package.json
						if len(npm.Browser) > 0 && task.Target != "deno" && task.Target != "denonext" && task.Target != "node" {
							spec := specifier
							if strings.HasPrefix(specifier, "./") || strings.HasPrefix(specifier, "../") || specifier == ".." {
								fullFilepath := filepath.Join(args.ResolveDir, specifier)
								spec = "." + strings.TrimPrefix(fullFilepath, path.Join(task.wd, "node_modules", npm.Name))
							}
							if name, ok := npm.Browser[spec]; ok {
								if name == "" {
									// browser exclude
									return api.OnResolveResult{Path: args.Path, Namespace: "browser-exclude"}, nil
								}
								if strings.HasPrefix(name, "./") {
									specifier = path.Join(task.wd, "node_modules", npm.Name, name)
								} else {
									specifier = name
								}
							}
						}

						// use `?alias` query
						if len(task.alias) > 0 {
							if name, ok := task.alias[specifier]; ok {
								specifier = name
							}
						}

						// bundles all dependencies in `bundle` mode, apart from peer dependencies and `?external` query
						if task.Bundle && !implicitExternal.Has(specifier) && !task.external.Has(specifier) {
							pkgName, _ := splitPkgPath(specifier)
							if !builtInNodeModules[pkgName] {
								_, ok := npm.PeerDependencies[pkgName]
								if !ok {
									return api.OnResolveResult{}, nil
								}
							}
						}

						// resolve path by `imports` of package.json
						if v, ok := npm.Imports[args.Path]; ok {
							if s, ok := v.(string); ok {
								return api.OnResolveResult{
									Path: path.Join(task.wd, "node_modules", npm.Name, s),
								}, nil
							} else if m, ok := v.(map[string]interface{}); ok {
								targets := []string{"browser", "default", "node"}
								if task.Target == "deno" || task.Target == "denonext" || task.Target == "node" {
									targets = []string{"node", "default", "browser"}
								}
								for _, t := range targets {
									if v, ok := m[t]; ok {
										if s, ok := v.(string); ok {
											return api.OnResolveResult{
												Path: path.Join(task.wd, "node_modules", npm.Name, s),
											}, nil
										}
									}
								}
							}
						}

						// externalize the main module
						// e.g. "react/jsx-runtime" imports "react"
						if task.Pkg.Submodule != "" && task.Pkg.Name == specifier {
							externalDeps.Add(specifier)
							return api.OnResolveResult{Path: "__ESM_SH_EXTERNAL:" + specifier, External: true}, nil
						}

						// bundle the package/module it self and the entrypoint
						if specifier == task.Pkg.ImportPath() || specifier == entryPoint || specifier == path.Join(npm.Name, npm.Main) || specifier == path.Join(npm.Name, npm.Module) {
							return api.OnResolveResult{}, nil
						}

						// splits modules based on the `exports` defines in package.json,
						// see https://nodejs.org/api/packages.html
						if (strings.HasPrefix(specifier, "./") || strings.HasPrefix(specifier, "../") || specifier == "..") && !strings.HasSuffix(specifier, ".js") && !strings.HasSuffix(specifier, ".mjs") && !strings.HasSuffix(specifier, ".json") {
							fullFilepath := filepath.Join(args.ResolveDir, specifier)
							spec := "." + strings.TrimPrefix(fullFilepath, path.Join(task.wd, "node_modules", npm.Name))
							// bundle {pkgName}/{pkgName}.js
							if spec == fmt.Sprintf("./%s.js", task.Pkg.Name) {
								return api.OnResolveResult{}, nil
							}
							v, ok := npm.DefinedExports.(map[string]interface{})
							if ok {
								for export, paths := range v {
									m, ok := paths.(map[string]interface{})
									if ok && export != "." {
										for _, value := range m {
											s, ok := value.(string)
											if ok && s != "" {
												match := spec == s || spec+".js" == s || spec+".mjs" == s
												if !match {
													if a := strings.Split(s, "*"); len(a) == 2 {
														prefix := a[0]
														suffix := a[1]
														if (strings.HasPrefix(spec, prefix)) &&
															(strings.HasSuffix(spec, suffix) ||
																strings.HasSuffix(spec+".js", suffix) ||
																strings.HasSuffix(spec+".mjs", suffix)) {
															matchName := strings.TrimPrefix(strings.TrimSuffix(spec, suffix), prefix)
															export = strings.Replace(export, "*", matchName, -1)
															match = true
														}
													}
												}
												if match {
													url := path.Join(npm.Name, export)
													if url == task.Pkg.ImportPath() {
														return api.OnResolveResult{}, nil
													}
													externalDeps.Add(url)
													return api.OnResolveResult{Path: "__ESM_SH_EXTERNAL:" + url, External: true}, nil
												}
											}
										}
									}
								}
							}
						}

						// for local modules
						if isLocalSpecifier(specifier) {
							// bundle if the entry pkg is not a submodule
							if task.Pkg.Submodule == "" {
								return api.OnResolveResult{}, nil
							}

							// bundle if this pkg has 'exports' definitions
							if npm.DefinedExports != nil && !reflect.ValueOf(npm.DefinedExports).IsNil() {
								return api.OnResolveResult{}, nil
							}

							// otherwise do not bundle its local dependencies
							fullFilepath := filepath.Join(args.ResolveDir, specifier)
							// convert: full filepath -> package name + submodule path
							specifier = strings.TrimPrefix(fullFilepath, filepath.Join(task.wd, "node_modules")+"/")
							externalDeps.Add(specifier)
							return api.OnResolveResult{Path: "__ESM_SH_EXTERNAL:" + specifier, External: true}, nil
						}

						// check dep `sideEffects`
						sideEffects := api.SideEffectsTrue
						if fileExists(path.Join(task.wd, "node_modules", specifier, "package.json")) {
							var np NpmPackage
							if utils.ParseJSONFile(path.Join(task.wd, "node_modules", specifier, "package.json"), &np) == nil {
								if !np.SideEffects {
									sideEffects = api.SideEffectsFalse
								}
							}
						}

						// dynamic external
						externalDeps.Add(specifier)
						return api.OnResolveResult{Path: "__ESM_SH_EXTERNAL:" + specifier, External: true, SideEffects: sideEffects}, nil
					},
				)

				// for browser exclude
				build.OnLoad(
					api.OnLoadOptions{Filter: ".*", Namespace: "browser-exclude"},
					func(args api.OnLoadArgs) (ret api.OnLoadResult, err error) {
						contents := "export default null;"
						if exports, ok := browserExclude[args.Path]; ok {
							for _, name := range exports.Values() {
								contents = fmt.Sprintf("%sexport const %s = null;", contents, name)
							}
						}
						return api.OnLoadResult{Contents: &contents, Loader: api.LoaderJS}, nil
					},
				)
			},
		}},
		Loader: map[string]api.Loader{
			".wasm":  api.LoaderDataURL,
			".svg":   api.LoaderDataURL,
			".png":   api.LoaderDataURL,
			".webp":  api.LoaderDataURL,
			".ttf":   api.LoaderDataURL,
			".eot":   api.LoaderDataURL,
			".woff":  api.LoaderDataURL,
			".woff2": api.LoaderDataURL,
		},
		SourceRoot: "/",
		Sourcemap:  api.SourceMapExternal,
	}
	if task.Target == "node" {
		options.Platform = api.PlatformNode
	} else {
		options.Define = define
	}
	if input != nil {
		options.Stdin = input
	} else if entryPoint != "" {
		options.EntryPoints = []string{entryPoint}
	}
	result := api.Build(options)
	if len(result.Errors) > 0 {
		// mark the missing module as external to exclude it from the bundle
		msg := result.Errors[0].Text
		if strings.HasPrefix(msg, "Could not resolve \"") {
			// current package/module can not be marked as external
			if strings.Contains(msg, fmt.Sprintf("Could not resolve \"%s\"", task.Pkg.ImportPath())) {
				err = fmt.Errorf("Could not resolve \"%s\"", task.Pkg.ImportPath())
				return
			}
			name := strings.Split(msg, "\"")[1]
			if !implicitExternal.Has(name) {
				implicitExternal.Add(name)
				externalDeps.Add(name)
				goto rebuild
			}
		}
		if strings.HasPrefix(msg, "No matching export in \"") {
			a := strings.Split(msg, "\"")
			if len(a) > 4 {
				path, exportName := a[1], a[3]
				if strings.HasPrefix(path, "browser-exclude:") && exportName != "default" {
					path = strings.TrimPrefix(path, "browser-exclude:")
					exports, ok := browserExclude[path]
					if !ok {
						exports = newStringSet()
						browserExclude[path] = exports
					}
					if !exports.Has(exportName) {
						exports.Add(exportName)
						goto rebuild
					}
				}
			}
		}
		err = errors.New("esbuild: " + msg)
		return
	}

	for _, w := range result.Warnings {
		if strings.HasPrefix(w.Text, "Could not resolve \"") {
			log.Warnf("esbuild(%s): %s", task.ID(), w.Text)
		}
	}

	for _, file := range result.OutputFiles {
		outputContent := file.Contents
		if strings.HasSuffix(file.Path, ".js") {
			buf := bytes.NewBufferString(fmt.Sprintf(
				"/* esm.sh - esbuild bundle(%s) %s %s */\n",
				task.Pkg.String(),
				strings.ToLower(task.Target),
				nodeEnv,
			))
			eol := "\n"
			if !task.Dev {
				eol = ""
			}

			// replace external imports/requires
			for depIndex, name := range externalDeps.Values() {
				var importPath string
				// remote imports
				if isRemoteSpecifier(name) || task.external.Has(name) {
					importPath = name
				}
				// sub module
				if importPath == "" && strings.HasPrefix(name, task.Pkg.Name+"/") {
					submodule := strings.TrimPrefix(name, task.Pkg.Name+"/")
					subPkg := Pkg{
						Name:      task.Pkg.Name,
						Version:   task.Pkg.Version,
						Submodule: submodule,
					}
					importPath = task.getImportPath(subPkg, encodeBuildArgsPrefix(task.BuildArgs, task.Pkg.Name, false))
				}
				// node builtin `buffer` module
				if importPath == "" && name == "buffer" {
					if task.Target == "node" || task.Target == "denonext" {
						importPath = "npm:buffer"
					} else {
						importPath = fmt.Sprintf("%s/v%d/node_buffer.js", cfg.BasePath, task.BuildVersion)
					}
				}
				// node builtin module
				if importPath == "" && builtInNodeModules[name] {
					if task.Target == "node" {
						importPath = fmt.Sprintf("node:%s", name)
					} else if task.Target == "denonext" && denoStdNodeModules[name] {
						if denoUnspportedNodeModules[name] {
							importPath = fmt.Sprintf("https://deno.land/std@%s/node/%s.ts", denoStdVersion, name)
						} else {
							importPath = fmt.Sprintf("node:%s", name)
						}
					} else if task.Target == "deno" && denoStdNodeModules[name] {
						importPath = fmt.Sprintf("https://deno.land/std@%s/node/%s.ts", task.denoStdVersion, name)
					} else {
						polyfill, ok := polyfilledBuiltInNodeModules[name]
						if ok {
							p, _, e := getPackageInfo(task.wd, polyfill, "latest")
							if e != nil {
								err = e
								return
							}
							importPath = task.getImportPath(Pkg{
								Name:    p.Name,
								Version: p.Version,
							}, "")
							extname := filepath.Ext(importPath)
							importPath = strings.TrimSuffix(importPath, extname) + ".bundle" + extname
						} else {
							_, err := embedFS.ReadFile(fmt.Sprintf("server/embed/polyfills/node_%s.js", name))
							if err == nil {
								importPath = fmt.Sprintf("%s/v%d/node_%s.js", cfg.BasePath, task.BuildVersion, name)
							} else {
								importPath = fmt.Sprintf(
									"%s/error.js?type=unsupported-nodejs-builtin-module&name=%s&importer=%s",
									cfg.BasePath,
									name,
									task.Pkg.Name,
								)
							}
						}
					}
				}
				// external all pattern
				if importPath == "" && task.external.Has("*") {
					importPath = name
				}
				// use `node_fetch.js` polyfill instead of `node-fetch`
				if importPath == "" && name == "node-fetch" && task.Target != "node" {
					importPath = fmt.Sprintf("%s/v%d/node_fetch.js", cfg.BasePath, task.BuildVersion)
				}
				// use version defined in `?deps` query
				if importPath == "" {
					for _, dep := range task.deps {
						if name == dep.Name || strings.HasPrefix(name, dep.Name+"/") {
							var submodule string
							if name != dep.Name {
								submodule = strings.TrimPrefix(name, dep.Name+"/")
							}
							subPkg := Pkg{
								Name:      dep.Name,
								Version:   dep.Version,
								Submodule: submodule,
							}
							importPath = task.getImportPath(subPkg, encodeBuildArgsPrefix(task.BuildArgs, subPkg.Name, false))
							break
						}
					}
				}
				// force the dependency version of `react` equals to react-dom
				if importPath == "" && task.Pkg.Name == "react-dom" && name == "react" {
					importPath = task.getImportPath(Pkg{
						Name:    name,
						Version: task.Pkg.Version,
					}, "")
				}
				// common npm dependency
				if importPath == "" {
					version := "latest"
					pkgName, submodule := splitPkgPath(name)
					if pkgName == task.Pkg.Name {
						version = task.Pkg.Version
					} else if v, ok := npm.Dependencies[pkgName]; ok {
						version = v
					} else if v, ok := npm.PeerDependencies[pkgName]; ok {
						version = v
					}
					p, _, e := getPackageInfo(task.wd, pkgName, version)
					if e != nil {
						err = e
						return
					}

					pkg := Pkg{
						Name:      p.Name,
						Version:   p.Version,
						Submodule: submodule,
					}
					t := &BuildTask{
						BuildArgs: BuildArgs{
							alias:             task.alias,
							deps:              task.deps,
							external:          task.external,
							treeShaking:       newStringSet(), // clear `?exports` args
							conditions:        newStringSet(),
							denoStdVersion:    task.denoStdVersion,
							ignoreRequire:     task.ignoreRequire,
							ignoreAnnotations: task.ignoreAnnotations,
							keepNames:         task.keepNames,
						},
						CdnOrigin:    task.CdnOrigin,
						BuildVersion: task.BuildVersion,
						Pkg:          pkg,
						Target:       task.Target,
						Dev:          task.Dev,
					}

					_, ok := queryESMBuild(t.ID())
					if ok {
						buildQueue.Add(t, "")
					}

					importPath = task.getImportPath(pkg, encodeBuildArgsPrefix(t.BuildArgs, pkg.Name, false))
				}
				if importPath == "" {
					err = fmt.Errorf("could not resolve \"%s\" (Imported by \"%s\")", name, task.Pkg.Name)
					return
				}

				buffer := bytes.NewBuffer(nil)
				identifier := fmt.Sprintf("%x", externalDeps.Size()-depIndex)
				cjsContext := false
				cjsImportNames := newStringSet()

				// walk output content to find all external dependencies
				slice := bytes.Split(outputContent, []byte(fmt.Sprintf("\"__ESM_SH_EXTERNAL:%s\"", name)))
				for i, p := range slice {
					if cjsContext {
						p = bytes.TrimPrefix(p, []byte{')'})
						var marked bool
						if builtInNodeModules[name] {
							cjsImportNames.Add("default")
							marked = true
						} else {
							depPkg, _, err := validatePkgPath(name)
							depWd := task.wd
							if err == nil && !fileExists(path.Join(depWd, "node_modules", depPkg.Name, "package.json")) {
								// the dep may be a peer depencency
								depWd = path.Join(os.TempDir(), fmt.Sprintf("esm/%s@%s", depPkg.Name, depPkg.Version))
								err = installPackage(depWd, depPkg)
							}
							if err == nil {
								task := &BuildTask{
									BuildArgs: task.BuildArgs,
									Pkg:       depPkg,
									Target:    task.Target,
									Dev:       task.Dev,
									wd:        depWd,
								}
								depESM, depNpm, e := task.init()
								if e == nil {
									// support edge case like `require('htmlparser').Parser`
									if bytes.HasPrefix(p, []byte{'.'}) {
										// right shift to strip the object `key`
										shift := 0
										for i, l := 1, len(p); i < l; i++ {
											c := p[i]
											if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$' {
												shift++
											} else {
												break
											}
										}
										importName := string(p[1 : shift+1])
										if includes(depESM.NamedExports, importName) {
											cjsImportNames.Add(importName)
											marked = true
											p = p[1:]
										} else {
											cjsImportNames.Add("default")
											marked = true
										}
									}
									// the dep is a esm module
									if !marked && depNpm.Module != "" {
										if depESM.HasExportDefault {
											cjsImportNames.Add("default")
										} else {
											cjsImportNames.Add("*")
										}
										marked = true
									}
									// the dep is a cjs module
									if !marked && depNpm.Module == "" {
										if includes(depESM.NamedExports, "__esModule") && depESM.HasExportDefault {
											cjsImportNames.Add("*")
										} else {
											cjsImportNames.Add("default")
										}
										marked = true
									}
								}
							}
						}
						if !marked {
							cjsImportNames.Add("lazy")
						}
					}
					cjsContext = bytes.HasSuffix(p, []byte{'('}) && !bytes.HasSuffix(p, []byte("import("))
					if cjsContext {
						// left shift to strip the `require` ident generated by esbuild
						shift := 0
						for i := len(p) - 2; i >= 0; i-- {
							c := p[i]
							if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$' {
								shift++
							} else {
								break
							}
						}
						if shift > 0 {
							p = p[0 : len(p)-(shift+1)]
						}
					}
					buffer.Write(p)
					if i < len(slice)-1 {
						if cjsContext {
							buffer.WriteString(fmt.Sprintf("__%s$", identifier))
						} else {
							buffer.WriteString(fmt.Sprintf("\"%s\"", importPath))
						}
					}
				}

				if cjsImportNames.Size() > 0 {
					buf := bytes.NewBuffer(nil)
					for _, importName := range cjsImportNames.Values() {
						if name == "object-assign" {
							fmt.Fprintf(buf, `const __%s$ = Object.assign;%s`, identifier, eol)
						} else {
							switch importName {
							case "lazy":
								fmt.Fprintf(buf, `import * as _%s$ from "%s";%s`, identifier, importPath, eol)
								fmt.Fprintf(buf, `const __%s$ = _%s$.default !== void 0 ? _%s$.default : _%s$;%s`, identifier, identifier, identifier, identifier, eol)
							case "default":
								fmt.Fprintf(buf, `import __%s$ from "%s";%s`, identifier, importPath, eol)
							case "*":
								fmt.Fprintf(buf, `import * as __%s$ from "%s";%s`, identifier, importPath, eol)
							default:
								fmt.Fprintf(buf, `import { %s as __%s$%s } from "%s";%s`, importName, identifier, importName, importPath, eol)
							}
						}
					}
					outputContent = make([]byte, buf.Len()+buffer.Len())
					copy(outputContent, buf.Bytes())
					copy(outputContent[buf.Len():], buffer.Bytes())
				} else {
					outputContent = buffer.Bytes()
				}
			}

			// add nodejs compatibility
			if task.Target != "node" {
				ids := newStringSet()
				for _, r := range regexpGlobalIdent.FindAll(outputContent, -1) {
					ids.Add(string(r))
				}
				if ids.Has("__Process$") {
					if task.Target == "denonext" {
						fmt.Fprintf(buf, `import __Process$ from "node:process";%s`, eol)
					} else if task.Target == "deno" {
						fmt.Fprintf(buf, `import __Process$ from "https://deno.land/std@%s/node/process.ts";%s`, task.denoStdVersion, eol)
					} else {
						fmt.Fprintf(buf, `import __Process$ from "%s/v%d/node_process.js";%s`, cfg.BasePath, task.BuildVersion, eol)
					}
				}
				if ids.Has("__Buffer$") {
					if task.Target == "denonext" {
						fmt.Fprintf(buf, `import { Buffer as __Buffer$ } from "node:buffer";%s`, eol)
					} else if task.Target == "deno" {
						fmt.Fprintf(buf, `import  { Buffer as __Buffer$ } from "https://deno.land/std@%s/node/buffer.ts";%s`, task.denoStdVersion, eol)
					} else {
						fmt.Fprintf(buf, `import { Buffer as __Buffer$ } from "%s/v%d/node_buffer.js";%s`, cfg.BasePath, task.BuildVersion, eol)
					}
				}
				if ids.Has("__global$") {
					fmt.Fprintf(buf, `var __global$ = globalThis || (typeof window !== "undefined" ? window : self);%s`, eol)
				}
				if ids.Has("__setImmediate$") {
					fmt.Fprintf(buf, `var __setImmediate$ = (cb, ...args) => setTimeout(cb, 0, ...args);%s`, eol)
				}
				if ids.Has("__rResolve$") {
					fmt.Fprintf(buf, `var __rResolve$ = p => p;%s`, eol)
				}
			}

			// most of npm packages check for window object to detect browser environment, but Deno also has the window object
			// so we need to replace the check with document object
			if task.Target == "deno" || task.Target == "denonext" {
				if task.Dev {
					outputContent = bytes.Replace(outputContent, []byte("typeof window !== \"undefined\""), []byte("typeof document !== \"undefined\""), -1)
				} else {
					outputContent = bytes.Replace(outputContent, []byte("typeof window<\"u\""), []byte("typeof document<\"u\""), -1)
				}
			}

			_, err = buf.Write(rewriteJS(task, outputContent))
			if err != nil {
				return
			}

			if task.Bundle && task.Target != "deno" && task.Target != "denonext" {
				options.Plugins = []api.Plugin{{
					Name: "esm",
					Setup: func(build api.PluginBuild) {
						build.OnResolve(
							api.OnResolveOptions{Filter: ".*"},
							func(args api.OnResolveArgs) (api.OnResolveResult, error) {
								var path string
								prefix := fmt.Sprintf(`%s/v%d/`, cfg.BasePath, task.BuildVersion)
								if strings.HasPrefix(args.Path, prefix) {
									path = "/" + strings.TrimPrefix(args.Path, prefix)
								} else if args.Namespace == "embed" {
									path = filepath.Join("/", args.Path)
								}
								data, err := embedFS.ReadFile(("server/embed/polyfills" + path))
								if err == nil {
									return api.OnResolveResult{
										Path:       path,
										Namespace:  "embed",
										PluginData: data,
									}, err
								}
								return api.OnResolveResult{
									Path:     args.Path,
									External: true,
								}, nil
							},
						)
						build.OnLoad(
							api.OnLoadOptions{Filter: ".*", Namespace: "embed"},
							func(args api.OnLoadArgs) (api.OnLoadResult, error) {
								data := args.PluginData.([]byte)
								contents := string(data)
								return api.OnLoadResult{
									Contents: &contents,
									Loader:   api.LoaderJS,
								}, nil
							},
						)
					},
				}}
				options.EntryPoints = nil
				options.Stdin = &api.StdinOptions{
					Contents:   buf.String(),
					ResolveDir: task.wd,
					Sourcefile: "index.js",
				}
				ret := api.Build(options)
				if len(ret.Errors) > 0 {
					msg := ret.Errors[0].Text
					err = errors.New("esbuild: " + msg)
					return
				}
				for _, w := range ret.Warnings {
					log.Warnf("esbuild(%s,bundler): %s", task.ID(), w.Text)
				}
				for _, file := range ret.OutputFiles {
					if strings.HasSuffix(file.Path, ".js") {
						buf.Reset()
						buf.Write(file.Contents)
					}
				}
			}

			// check if package is deprecated
			p, e := fetchPackageInfo(task.Pkg.Name, task.Pkg.Version)
			if e == nil && p.Deprecated != "" {
				fmt.Fprintf(buf, `console.warn("[npm] %%cdeprecated%%c %s@%s: %s", "color:red", "");%s`, task.Pkg.Name, task.Pkg.Version, p.Deprecated, "\n")
			}

			// add sourcemap Url
			buf.WriteString("//# sourceMappingURL=")
			buf.WriteString(filepath.Base(task.ID()))
			buf.WriteString(".map")

			_, err = fs.WriteFile(task.getSavepath(), buf)
			if err != nil {
				return
			}
		} else if strings.HasSuffix(file.Path, ".css") {
			savePath := task.getSavepath()
			_, err = fs.WriteFile(strings.TrimSuffix(savePath, path.Ext(savePath))+".css", bytes.NewReader(outputContent))
			if err != nil {
				return
			}
			esm.PackageCSS = true
		} else if strings.HasSuffix(file.Path, ".map") {
			_, err = fs.WriteFile(task.getSavepath()+".map", bytes.NewReader(outputContent))
			if err != nil {
				return
			}
		}
	}

	task.checkDTS(esm, npm)
	task.storeToDB(esm)
	return
}

func (task *BuildTask) storeToDB(esm *ESMBuild) {
	err := db.Put(task.ID(), utils.MustEncodeJSON(esm))
	if err != nil {
		log.Errorf("db: %v", err)
	}
}

func (task *BuildTask) checkDTS(esm *ESMBuild, npm NpmPackage) {
	name := task.Pkg.Name
	submodule := task.Pkg.Submodule
	var dts string
	if npm.Types != "" {
		dts = toTypesPath(task.wd, npm, "", encodeBuildArgsPrefix(task.BuildArgs, task.Pkg.Name, true), submodule)
	} else if !strings.HasPrefix(name, "@types/") {
		versions := []string{"latest"}
		versionParts := strings.Split(task.Pkg.Version, ".")
		if len(versionParts) > 2 {
			versions = []string{
				"~" + strings.Join(versionParts[:2], "."), // minor
				"^" + versionParts[0],                     // major
				"latest",
			}
		}
		typesPkgName := toTypesPackageName(name)
		pkg, ok := task.deps.Get(typesPkgName)
		if ok {
			// use the version of the `?deps` query if it exists
			versions = append([]string{pkg.Version}, versions...)
		}
		for _, version := range versions {
			p, _, err := getPackageInfo(task.wd, typesPkgName, version)
			if err == nil {
				prefix := encodeBuildArgsPrefix(task.BuildArgs, p.Name, true)
				dts = toTypesPath(task.wd, p, version, prefix, submodule)
				break
			}
		}
	}
	if dts != "" {
		esm.Dts = fmt.Sprintf("/v%d/%s", task.BuildVersion, dts)
	}
}

func (task *BuildTask) buildDTS(dts string) {
	start := time.Now()
	n, err := task.TransformDTS(dts)
	if err != nil && os.IsExist(err) {
		log.Errorf("TransformDTS(%s): %v", dts, err)
		return
	}
	log.Debugf("transform dts '%s' (%d related dts files transformed) in %v", dts, n, time.Since(start))
}
