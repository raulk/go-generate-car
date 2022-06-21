package util

import (
	"context"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dss "github.com/ipfs/go-datastore/sync"
	"github.com/ipfs/go-filestore"
	bstore "github.com/ipfs/go-ipfs-blockstore"
	chunker "github.com/ipfs/go-ipfs-chunker"
	offline "github.com/ipfs/go-ipfs-exchange-offline"
	files "github.com/ipfs/go-ipfs-files"
	format "github.com/ipfs/go-ipld-format"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"
	"github.com/ipfs/go-merkledag"
	dag "github.com/ipfs/go-merkledag"
	"github.com/ipfs/go-unixfs"
	"github.com/ipfs/go-unixfs/importer/balanced"
	ihelper "github.com/ipfs/go-unixfs/importer/helpers"
	"github.com/ipld/go-car"
	ipldprime "github.com/ipld/go-ipld-prime"
	basicnode "github.com/ipld/go-ipld-prime/node/basic"
	"github.com/ipld/go-ipld-prime/traversal/selector"
	"github.com/ipld/go-ipld-prime/traversal/selector/builder"
	"golang.org/x/xerrors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const UnixfsLinksPerLevel = 1 << 10
const UnixfsChunkSize uint64 = 1 << 20

var logger = logging.Logger("graphsplit")

type FSBuilder struct {
	root *dag.ProtoNode
	ds   ipld.DAGService
}

func getDirKey(dirList []string, i int) (key string) {
	for j := 0; j <= i; j++ {
		key += dirList[j]
		if j < i {
			key += "."
		}
	}
	return
}
func NewFSBuilder(root *dag.ProtoNode, ds ipld.DAGService) *FSBuilder {
	return &FSBuilder{root, ds}
}

func isLinked(node *dag.ProtoNode, name string) bool {
	for _, lk := range node.Links() {
		if lk.Name == name {
			return true
		}
	}
	return false
}

type Finfo struct {
	Path  string
	Size  int64
	Start int64
	End   int64
}

type fileSlice struct {
	r        *os.File
	offset   int64
	start    int64
	end      int64
	fileSize int64
}

func (fs *fileSlice) Close() error {
	return fs.Close()
}

func (fs *fileSlice) Read(p []byte) (n int, err error) {
	if fs.end == 0 {
		fs.end = fs.fileSize
	}
	if fs.offset == 0 && fs.start > 0 {
		_, err = fs.r.Seek(fs.start, 0)
		if err != nil {
			logger.Warn(err)
			return 0, err
		}
		fs.offset = fs.start
	}
	if fs.end-fs.offset == 0 {
		return 0, io.EOF
	}
	if fs.end-fs.offset < 0 {
		return 0, xerrors.Errorf("read data out bound of the slice")
	}
	plen := len(p)
	leftLen := fs.end - fs.offset
	if leftLen > int64(plen) {
		n, err = fs.r.Read(p)
		if err != nil {
			return
		}
		fs.offset += int64(n)
		return
	}
	b := make([]byte, leftLen)
	n, err = fs.r.Read(b)
	if err != nil {
		return
	}
	fs.offset += int64(n)

	return copy(p, b), io.EOF
}

func GenerateCar(ctx context.Context, fileList []Finfo, parentPath string, tmpDir string, output io.Writer) (ipldDag *FsNode, cid string, err error) {
	batching := dss.MutexWrap(datastore.NewMapDatastore())
	bs1 := bstore.NewBlockstore(batching)
	absParentPath, err := filepath.Abs(parentPath)
	if err != nil {
		logger.Warn(err)
		return
	}
	if tmpDir != "" {
		absParentPath, err = filepath.Abs(tmpDir)
		if err != nil {
			logger.Warn(err)
			return
		}
	}
	fm := filestore.NewFileManager(batching, absParentPath)
	fm.AllowFiles = true
	bs2 := filestore.NewFilestore(bs1, fm)
	dagServ := merkledag.NewDAGService(blockservice.New(bs2, offline.Exchange(bs2)))
	cidBuilder, err := merkledag.PrefixForCidVersion(1)
	if err != nil {
		logger.Warn(err)
		return
	}
	layers := []ipld.Node{}
	rootNode := unixfs.EmptyDirNode()
	rootNode.SetCidBuilder(cidBuilder)
	layers = append(layers, rootNode)
	previous := []string{""}
	for _, item := range fileList {
		if item.End == 0 {
			item.End = item.Size
		}
		var node ipld.Node
		var path string
		path, err = filepath.Rel(filepath.Clean(parentPath), filepath.Clean(item.Path))
		if tmpDir != "" {
			tmpPath := filepath.Join(filepath.Clean(tmpDir), path)
			err = os.MkdirAll(filepath.Dir(tmpPath), 0777)
			if err != nil {
				logger.Warn(err)
				return
			}
			// copy file
			source, err := os.Open(item.Path)
			if err != nil {
				logger.Warn(err)
				return nil, "", err
			}
			defer source.Close()
			destination, err := os.Create(tmpPath)
			if err != nil {
				logger.Warn(err)
				return nil, "", err
			}
			defer destination.Close()
			_, err = source.Seek(item.Start, 0)
			if err != nil {
				logger.Warn(err)
				return nil, "", err
			}
			_, err = io.CopyN(destination, source, item.End-item.Start)
			if err != nil {
				logger.Warn(err)
				return nil, "", err
			}
			item.Path = tmpPath
			item.Size = item.End - item.Start
			item.End = item.Size
			item.Start = 0
		}
		node, err = BuildFileNode(item, dagServ, cidBuilder)
		if err != nil {
			logger.Warn(err)
			return
		}
		if err != nil {
			logger.Warn(err)
			return
		}
		current := append([]string{""}, strings.Split(path, string(filepath.Separator))...)
		// Find the common prefix
		i := 0
		var minLength int
		if len(previous) < len(current) {
			minLength = len(previous)
		} else {
			minLength = len(current)
		}
		for ; i < minLength; i++ {
			if previous[i] != current[i] {
				break
			}
		}
		for j := len(previous) - 1; j >= i; j-- {
			lastNode := layers[len(layers)-1]
			lastName := previous[len(previous)-1]
			layers = layers[:len(layers)-1]
			previous = previous[:len(previous)-1]
			if j != len(previous)-1 {
				dagServ.Add(ctx, lastNode)
			}
			dirNode, ok := layers[len(layers)-1].(*dag.ProtoNode)
			if !ok {
				err = xerrors.Errorf("node is not proto node")
				logger.Warn(err)
				return
			}
			dirNode.AddNodeLink(lastName, lastNode)
		}
		for j := i; j < len(current); j++ {
			if j == len(current)-1 {
				layers = append(layers, node)
			} else {
				newNode := unixfs.EmptyDirNode()
				newNode.SetCidBuilder(cidBuilder)
				layers = append(layers, newNode)
			}
		}
		previous = current
	}
	for j := len(previous) - 1; j >= 1; j-- {
		lastNode := layers[len(layers)-1]
		lastName := previous[len(previous)-1]
		layers = layers[:len(layers)-1]
		previous = previous[:len(previous)-1]
		if j != len(previous)-1 {
			dagServ.Add(ctx, lastNode)
		}
		dirNode, ok := layers[len(layers)-1].(*dag.ProtoNode)
		if !ok {
			err = xerrors.Errorf("node is not proto node")
			logger.Warn(err)
			return
		}
		dirNode.AddNodeLink(lastName, lastNode)
	}
	dagServ.Add(ctx, rootNode)
	selector := allSelector()
	sc := car.NewSelectiveCar(ctx, bs2, []car.Dag{{Root: rootNode.Cid(), Selector: selector}})
	err = sc.Write(output)
	if err != nil {
		logger.Warn(err)
		return
	}
	fsBuilder := NewFSBuilder(rootNode, dagServ)
	ipldDag, err = fsBuilder.Build()
	cid = rootNode.Cid().String()
	return
}

func allSelector() ipldprime.Node {
	ssb := builder.NewSelectorSpecBuilder(basicnode.Prototype.Any)
	return ssb.ExploreRecursive(selector.RecursionLimitNone(),
		ssb.ExploreAll(ssb.ExploreRecursiveEdge())).
		Node()
}
func BuildFileNode(item Finfo, bufDs ipld.DAGService, cidBuilder cid.Builder) (node ipld.Node, err error) {
	f, err := os.Open(item.Path)
	if err != nil {
		logger.Warn(err)
		return
	}
	var r io.Reader
	if item.Start == 0 && item.End == item.Size {
		r, err = files.NewReaderPathFile(item.Path, f, nil)
	} else {
		r, err = files.NewReaderPathFile(item.Path, &fileSlice{
			r:        f,
			start:    item.Start,
			end:      item.End,
			fileSize: item.Size,
		}, nil)
	}
	if err != nil {
		logger.Warn(err)
		return
	}

	params := ihelper.DagBuilderParams{
		Maxlinks:   UnixfsLinksPerLevel,
		RawLeaves:  true,
		CidBuilder: cidBuilder,
		Dagserv:    bufDs,
		NoCopy:     true,
	}
	db, err := params.New(chunker.NewSizeSplitter(r, int64(UnixfsChunkSize)))
	db.SetOffset(uint64(item.Start))
	if err != nil {
		logger.Warn(err)
		return
	}
	node, err = balanced.Layout(db)
	if err != nil {
		logger.Warn(err)
		return
	}
	return
}
func (b *FSBuilder) Build() (rootn *FsNode, err error) {
	fsn, err := unixfs.FSNodeFromBytes(b.root.Data())
	if err != nil {
		return nil, xerrors.Errorf("input dag is not a unixfs node: %s", err)
	}

	rootn = &FsNode{
		Hash: b.root.Cid().String(),
		Size: fsn.FileSize(),
		Link: []FsNode{},
	}
	if !fsn.IsDir() {
		return rootn, nil
	}
	for _, ln := range b.root.Links() {
		var fn FsNode
		fn, err = b.getNodeByLink(ln)
		if err != nil {
			logger.Warn(err)
			return
		}
		rootn.Link = append(rootn.Link, fn)
	}

	return rootn, nil
}

type FsNode struct {
	Name string
	Hash string
	Size uint64
	Link []FsNode
}

func (b *FSBuilder) getNodeByLink(ln *format.Link) (fn FsNode, err error) {
	ctx := context.Background()
	fn = FsNode{
		Name: ln.Name,
		Hash: ln.Cid.String(),
		Size: ln.Size,
	}
	nd, err := b.ds.Get(ctx, ln.Cid)
	if err != nil {
		logger.Warn(err)
		return
	}

	nnd, ok := nd.(*dag.ProtoNode)
	if !ok {
		// format.Node | merkeldag.RawNode
		return
	}
	fsn, err := unixfs.FSNodeFromBytes(nnd.Data())
	if err != nil {
		logger.Warn("input dag is not a unixfs node: %s", err)
		return
	}
	if !fsn.IsDir() {
		return
	}
	for _, ln := range nnd.Links() {
		node, err := b.getNodeByLink(ln)
		if err != nil {
			return node, err
		}
		fn.Link = append(fn.Link, node)
	}
	return
}
