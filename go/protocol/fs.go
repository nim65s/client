// Auto-generated by avdl-compiler v1.3.1 (https://github.com/keybase/node-avdl-compiler)
//   Input file: avdl/fs.avdl

package keybase1

import (
	rpc "github.com/keybase/go-framed-msgpack-rpc"
	context "golang.org/x/net/context"
)

type File struct {
	Name string `codec:"name" json:"name"`
}

type FsListResult struct {
	Files []File `codec:"files" json:"files"`
}

type FsListArg struct {
	SessionID int    `codec:"sessionID" json:"sessionID"`
	Path      string `codec:"path" json:"path"`
}

type FsInterface interface {
	// List files in a path. Implemented by KBFS service.
	FsList(context.Context, FsListArg) (FsListResult, error)
}

func FsProtocol(i FsInterface) rpc.Protocol {
	return rpc.Protocol{
		Name: "keybase.1.fs",
		Methods: map[string]rpc.ServeHandlerDescription{
			"fsList": {
				MakeArg: func() interface{} {
					ret := make([]FsListArg, 1)
					return &ret
				},
				Handler: func(ctx context.Context, args interface{}) (ret interface{}, err error) {
					typedArgs, ok := args.(*[]FsListArg)
					if !ok {
						err = rpc.NewTypeError((*[]FsListArg)(nil), args)
						return
					}
					ret, err = i.FsList(ctx, (*typedArgs)[0])
					return
				},
				MethodType: rpc.MethodCall,
			},
		},
	}
}

type FsClient struct {
	Cli rpc.GenericClient
}

// List files in a path. Implemented by KBFS service.
func (c FsClient) FsList(ctx context.Context, __arg FsListArg) (res FsListResult, err error) {
	err = c.Cli.Call(ctx, "keybase.1.fs.fsList", []interface{}{__arg}, &res)
	return
}