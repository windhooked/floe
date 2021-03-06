package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/floeit/floe/config"
	"github.com/floeit/floe/event"
	"github.com/floeit/floe/hub"
	"github.com/floeit/floe/log"
	"github.com/floeit/floe/path"
	"github.com/floeit/floe/server"
	"github.com/floeit/floe/store"
)

func main() {
	c := srvConf{}
	flag.StringVar(&c.ConfFile, "conf", "config.yml", "the host config yaml")
	flag.StringVar(&c.HostName, "host_name", "h1", "a short host name to use in id creation and routing")
	flag.StringVar(&c.AdminToken, "admin", "", "admin token to share in a cluster to confirm it's a p2p call")
	flag.StringVar(&c.Tags, "tags", "master", "host tags")

	flag.StringVar(&c.PubBind, "pub_bind", ":443", "what to bind the public server to")
	flag.StringVar(&c.PubCert, "pub_cert", "", "public certificate path")
	flag.StringVar(&c.PubKey, "pub_key", "", "key path for the public endpoint")

	flag.StringVar(&c.PrvBind, "prv_bind", "", "what to bind the private server to")
	flag.StringVar(&c.PrvCert, "prv_cert", "", "private certificate path")
	flag.StringVar(&c.PrvKey, "prv_key", "", "key path for the private endpoint")

	flag.BoolVar(&c.WebDev, "dev", false, "set to true to use local webapp folder during development")

	flag.Parse()

	cfg, err := ioutil.ReadFile(c.ConfFile)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}

	log.Error(start(c, cfg, nil))
}

type srvConf struct {
	server.Conf

	ConfFile   string // the path to the main config file
	HostName   string // the name of this host
	AdminToken string // the token to use to verify nodes in the cluster
	Tags       string // tags for this server to be matched against tags specified in the flows

	WebDev bool // use local file system for web assets
}

// %store_root%/store         "~/.floe/store"
// %workspace_root%/spaces    "~/.floe/spaces"

func start(sc srvConf, conf []byte, addr chan string) error {

	c, err := config.ParseYAML(conf)
	if err != nil {
		return err
	}

	var s store.Store
	switch c.Common.StoreType {
	case "", "memory":
		s = store.NewMemStore()
	case "local":
		root, err := path.Expand(c.Common.StoreRoot)
		if err != nil {
			return err
		}
		s, err = store.NewLocalStore(filepath.Join(root, "store"))
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s is not a supported store", c.Common.StoreType)
	}
	// TODO - implement other stores e.g. s3

	q := &event.Queue{}
	hub := hub.New(sc.HostName, sc.Tags, sc.AdminToken, c, s, q)
	server.AdminToken = sc.AdminToken

	server.LaunchWeb(sc.Conf, c.Common.BaseURL, hub, q, addr, sc.WebDev)
	return nil
}
