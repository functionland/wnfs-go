package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"
	cid "github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	golog "github.com/ipfs/go-log"
	wnfs "github.com/functionland/wnfs-go"
	base "github.com/functionland/wnfs-go/base"
	fsdiff "github.com/functionland/wnfs-go/fsdiff"
	gateway "github.com/functionland/wnfs-go/gateway"
	public "github.com/functionland/wnfs-go/public"
	cli "github.com/urfave/cli/v2"
)

func init() {
	if lvl := os.Getenv("WNFS_LOGGING"); lvl != "" {
		golog.SetLogLevel("wnfs", lvl)
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var repo *Repo

	app := &cli.App{
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"v"},
				Usage:   "print verbose output",
			},
		},
		Before: func(c *cli.Context) (err error) {
			if c.Bool("verbose") {
				golog.SetLogLevel("wnfs", "debug")
			}

			repo, err = OpenRepo(ctx)
			return err
		},
		Commands: []*cli.Command{
			// read commands
			{
				Name:  "cat",
				Usage: "cat a file",
				Action: func(c *cli.Context) error {
					fs := repo.WNFS()
					data, err := fs.Cat(c.Args().Get(0))
					if err != nil {
						return err
					}
					_, err = os.Stdout.Write(data)
					return err
				},
			},
			{
				Name:  "stat",
				Usage: "get stats for a file",
				Action: func(c *cli.Context) error {
					fsys := repo.WNFS()
					f, err := fsys.Open(c.Args().Get(0))
					if err != nil {
						return nil
					}
					n, ok := f.(wnfs.Node)
					if !ok {
						return fmt.Errorf("wnfs did not return a node")
					}
					fmt.Printf(`
cid:	%s
type:	%s
size:	%d
`[1:], n.Cid(), n.Type(), n.Size())

					return nil
				},
			},
			{
				Name:  "ls",
				Usage: "list the contents of a directory",
				Action: func(c *cli.Context) error {
					fs := repo.WNFS()
					entries, err := fs.Ls(c.Args().Get(0))
					if err != nil {
						return err
					}

					for _, entry := range entries {
						fmt.Println(entry.Name())
					}
					return nil
				},
			},
			{
				Name:  "tree",
				Usage: "show a tree rooted at a given path",
				Action: func(c *cli.Context) error {
					path := c.Args().Get(0)
					fs := repo.WNFS()
					// TODO(b5): can't yet create tree from wnfs root.
					// for now replace empty string with "public"
					if path == "" {
						path = "."
					}

					s, err := treeString(fs, path)
					if err != nil {
						return err
					}

					os.Stdout.Write([]byte(s))
					return nil
				},
			},
			{
				Name:    "log",
				Aliases: []string{"history"},
				Usage:   "show the history of a path",
				Action: func(c *cli.Context) error {
					fs := repo.WNFS()
					entries, err := fs.History(context.TODO(), c.Args().Get(0), -1)
					if err != nil {
						return err
					}

					fmt.Println("date\tsize\tcid\tkey\tprivate name")
					for _, entry := range entries {
						ts := time.Unix(entry.Mtime, 0)
						fmt.Printf("%s\t%s\t%s\t%s\t%s\n", ts.Format(time.RFC3339), humanize.Bytes(uint64(entry.Size)), entry.Cid, entry.Key, entry.PrivateName)
					}
					return nil
				},
			},
			{
				Name:  "diff",
				Usage: "",
				Action: func(c *cli.Context) error {
					cmdCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					fs := repo.WNFS()

					entries, err := fs.History(context.TODO(), ".", 2)
					if err != nil {
						return err
					}
					if len(entries) < 2 {
						fmt.Println("no history")
						return nil
					}

					key := &wnfs.Key{}
					if err := key.Decode(entries[1].Key); err != nil {
						return err
					}

					prev, err := wnfs.FromCID(cmdCtx, repo.Store().Blockservice(), repo.RatchetStore(), entries[1].Cid, *key, wnfs.PrivateName(entries[1].PrivateName))
					if err != nil {
						errExit("error: opening previous WNFS %s:\n%s\n", entries[1].Cid, err.Error())
					}

					diff, err := fsdiff.Unix("", "", prev, fs)
					if err != nil {
						errExit("error: constructing diff: %s", err)
					}

					fmt.Println(fsdiff.PrettyPrintFileDiffs(diff))
					return nil
				},
			},

			// write commands
			{
				Name:  "mkdir",
				Usage: "create a directory",
				Action: func(c *cli.Context) error {
					fs := repo.WNFS()
					if err := fs.Mkdir(c.Args().Get(0)); err != nil {
						return err
					}
					return repo.Commit(fs)
				},
			},
			{
				Name:    "write",
				Aliases: []string{"add"},
				Usage:   "add a file to wnfs",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "meta",
						Value: "",
						Usage: "json file of metadata",
					},
				},
				Action: func(c *cli.Context) error {
					path := c.Args().Get(0)
					file := c.Args().Get(1)

					var f fs.File
					f, err := os.Open(file)
					if err != nil {
						return err
					}

					if metaPath := c.String("meta"); metaPath != "" {
						var meta interface{}
						d, err := ioutil.ReadFile(metaPath)
						if err != nil {
							return err
						}
						if err = json.Unmarshal(d, &meta); err != nil {
							return err
						}
						f = public.WrapFileMetadata(f, meta)
					}

					if filepath.Ext(file) == ".json" {
						var v interface{}
						if err = json.NewDecoder(f).Decode(&v); err != nil {
							return err
						}
						f = wnfs.NewLDFile(filepath.Base(path), v)
					}

					fs := repo.WNFS()
					if err := fs.Write(path, f); err != nil {
						return err
					}
					return repo.Commit(fs)
				},
			},
			{
				Name:    "copy",
				Aliases: []string{"cp"},
				Usage:   "copy a file or directory into wnfs",
				Action: func(c *cli.Context) error {
					wnfsPath := c.Args().Get(0)
					localPath, err := filepath.Abs(c.Args().Get(1))
					if err != nil {
						return err
					}

					localFS := os.DirFS(filepath.Dir(localPath))
					path := filepath.Base(localPath)

					fs := repo.WNFS()
					if err = fs.Cp(wnfsPath, path, localFS); err != nil {
						return err
					}
					return repo.Commit(fs)
				},
			},
			{
				Name:  "rm",
				Usage: "remove files and directories",
				Action: func(c *cli.Context) error {
					fs := repo.WNFS()
					if err := fs.Rm(c.Args().Get(0)); err != nil {
						return err
					}
					return repo.Commit(fs)
				},
			},
			{
				Name:  "merge",
				Usage: "",
				Action: func(c *cli.Context) error {
					a := repo.WNFS()
					cmdCtx, cancel := context.WithCancel(ctx)
					defer cancel()

					path := c.Args().Get(0)
					if filepath.Base(path) != repoDirname {
						path = filepath.Join(path, repoDirname)
					}
					fmt.Printf("reading wnfs repo from %q ...", path)
					bRepo, err := OpenRepoPath(cmdCtx, path)
					if err != nil {
						return err
					}
					b := bRepo.WNFS()
					fmt.Printf("done\n")

					if err = wnfs.Merge(cmdCtx, a, b); err != nil {
						return err
					}
					return repo.Commit(a)
				},
			},

			// metadata commands
			{
				Name:  "meta",
				Usage: "",
				Subcommands: []*cli.Command{
					{
						Name:  "set",
						Usage: "",
						Action: func(c *cli.Context) error {
							fs := repo.WNFS()
							path := c.Args().Get(0)
							metaPath := c.Args().Get(1)

							d, err := ioutil.ReadFile(metaPath)
							if err != nil {
								return err
							}

							// TODO(b5): this needs to be replaced by a wnfs parser that
							// understands links
							var meta interface{}
							if err = json.Unmarshal(d, &meta); err != nil {
								return err
							}

							f, err := fs.Open(path)
							if err != nil {
								return err
							}
							if err = f.(base.WritableMetaNode).SetMetadata(meta); err != nil {
								return err
							}
							return repo.Commit(fs)
						},
					},
					{
						Name:  "get",
						Usage: "",
						Action: func(c *cli.Context) error {
							fs := repo.WNFS()
							path := c.Args().Get(0)

							f, err := fs.Open(path)
							if err != nil {
								return err
							}

							mdFile, err := f.(wnfs.Node).Metadata()
							if err != nil {
								return err
							}

							metaJSON, err := ioutil.ReadAll(mdFile)
							if err != nil {
								return err
							}

							fmt.Println(string(metaJSON))
							return nil
						},
					},
				},
			},

			// HTTP gateway
			{
				Name:  "gateway",
				Usage: "",
				Action: func(c *cli.Context) error {
					s := &gateway.Server{
						Factory: repo.Factory(),
					}

					port := c.Args().Get(0)
					if port == "" {
						port = ":8080"
					} else if !strings.HasPrefix(port, ":") {
						port = ":" + port
					}

					return s.Serve(port)
				},
			},

			// plumbing & diagnostic commands
			{
				Name: "block",
				Subcommands: []*cli.Command{
					{
						Name:  "list",
						Usage: "",
						Action: func(c *cli.Context) error {
							cmdCtx, cancel := context.WithCancel(ctx)
							defer cancel()
							keys, err := repo.Store().Blockservice().Blockstore().AllKeysChan(cmdCtx)
							if err != nil {
								return err
							}
							for key := range keys {
								fmt.Println(key)
							}
							return nil
						},
					},
					{
						Name:  "get",
						Usage: "",
						Flags: []cli.Flag{
							&cli.BoolFlag{
								Name:  "raw",
								Value: false,
								Usage: "don't decode CBOR nodes to JSON",
							},
						},
						Action: func(c *cli.Context) error {
							cmdCtx, cancel := context.WithCancel(ctx)
							defer cancel()

							id, err := cid.Parse(c.Args().Get(0))
							if err != nil {
								return err
							}

							blk, err := repo.Store().Blockservice().GetBlock(cmdCtx, id)
							if err != nil {
								return err
							}
							if c.Bool("raw") {
								fmt.Printf("%x\n", blk.RawData())
								return nil
							}

							v := map[string]interface{}{}
							if err := cbornode.DecodeInto(blk.RawData(), &v); err != nil {
								return err
							}
							d, err := json.Marshal(v)
							if err != nil {
								return err
							}
							fmt.Println(string(d))
							return nil
						},
					},
				},
			},
			{
				Name: "hamt",
				Action: func(c *cli.Context) error {
					cmdCtx, cancel := context.WithCancel(ctx)
					defer cancel()

					id, err := cid.Parse(c.Args().Get(0))
					if err != nil {
						return err
					}

					diag, err := wnfs.HAMTContents(cmdCtx, repo.Store().Blockservice(), id)
					if err != nil {
						return err
					}
					for k, v := range diag {
						fmt.Printf("%s\t%s\n", k, v)
					}
					return nil
				},
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		errExit(err.Error())
	}
}

func errExit(msg string, v ...interface{}) {
	fmt.Printf(msg, v...)
	os.Exit(1)
}
