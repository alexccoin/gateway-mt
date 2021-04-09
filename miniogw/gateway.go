// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package miniogw

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/spacemonkeygo/monkit/v3"
	minio "github.com/storj/minio/cmd"
	"github.com/storj/minio/cmd/logger"
	"github.com/storj/minio/pkg/auth"
	"github.com/storj/minio/pkg/hash"
	"github.com/zeebo/errs"

	"storj.io/common/errs2"
	"storj.io/common/rpc/rpcpool"
	"storj.io/common/rpc/rpcstatus"
	"storj.io/common/storj"
	"storj.io/private/version"
	"storj.io/uplink"
	"storj.io/uplink/private/storage/streams"
	"storj.io/uplink/private/transport"
)

var (
	mon              = monkit.Package()
	gatewayUserAgent = "Gateway-MT/" + version.Build.Version.String()

	// Error is the errs class of standard End User Client errors.
	Error = errs.Class("Storj Gateway error")
)

// NewStorjGateway creates a new Storj S3 gateway.
func NewStorjGateway(config uplink.Config, connectionPool *rpcpool.Pool, multipartSatAddrs []string) *Gateway {
	m := make(map[string]struct{}, len(multipartSatAddrs))
	for _, sat := range multipartSatAddrs {
		m[sat] = struct{}{}
	}
	return &Gateway{
		config:            config,
		connectionPool:    connectionPool,
		multipartSatAddrs: m,
	}
}

// Gateway is the implementation of a minio cmd.Gateway.
type Gateway struct {
	config            uplink.Config
	connectionPool    *rpcpool.Pool
	multipartSatAddrs map[string]struct{}
}

// Name implements cmd.Gateway.
func (gateway *Gateway) Name() string {
	return "storj"
}

// NewGatewayLayer implements cmd.Gateway.
func (gateway *Gateway) NewGatewayLayer(creds auth.Credentials) (minio.ObjectLayer, error) {
	return &gatewayLayer{
		gateway: gateway,
	}, nil
}

// Production implements cmd.Gateway.
func (gateway *Gateway) Production() bool {
	return version.Build.Release
}

type gatewayLayer struct {
	minio.GatewayUnsupported
	gateway *Gateway
}

func (layer *gatewayLayer) DeleteBucket(ctx context.Context, bucketName string, forceDelete bool) (err error) {
	defer mon.Task()(&ctx)(&err)

	project, err := layer.openProject(ctx, getAccessGrant(ctx))
	if err != nil {
		return convertError(err, bucketName, "")
	}
	defer func() {
		err = errs.Combine(err, project.Close())
	}()

	if forceDelete {
		_, err = project.DeleteBucketWithObjects(ctx, bucketName)
		return convertError(err, bucketName, "")
	}

	_, err = project.DeleteBucket(ctx, bucketName)
	return convertError(err, bucketName, "")
}

func (layer *gatewayLayer) DeleteObject(ctx context.Context, bucketName, objectPath string, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	project, err := layer.openProject(ctx, getAccessGrant(ctx))
	if err != nil {
		return minio.ObjectInfo{}, err
	}
	defer func() {
		err = errs.Combine(err, project.Close())
	}()

	// TODO this should be removed and implemented on satellite side.
	// This call needs to occur prior to the DeleteObject call below, because
	// project.DeleteObject will return a nil error for a missing bucket. To
	// maintain consistency, we need to manually check if the bucket exists.
	_, err = project.StatBucket(ctx, bucketName)
	if err != nil {
		return minio.ObjectInfo{}, convertError(err, bucketName, objectPath)
	}

	object, err := project.DeleteObject(ctx, bucketName, objectPath)
	if err != nil {
		return minio.ObjectInfo{}, convertError(err, bucketName, objectPath)
	}

	return minioObjectInfo(bucketName, "", object), nil
}

func (layer *gatewayLayer) DeleteObjects(ctx context.Context, bucketName string, objects []minio.ObjectToDelete, opts minio.ObjectOptions) (deleted []minio.DeletedObject, errs []error) {
	// TODO: implement multiple object deletion in libuplink API
	errs = make([]error, len(objects))
	deleted = make([]minio.DeletedObject, len(objects))
	for i, object := range objects {
		_, deleteErr := layer.DeleteObject(ctx, bucketName, object.ObjectName, opts)
		if deleteErr != nil && !errors.As(deleteErr, &minio.ObjectNotFound{}) {
			errs[i] = convertError(deleteErr, bucketName, object.ObjectName)
			continue
		}
		deleted[i].ObjectName = object.ObjectName
	}
	return deleted, errs
}

func (layer *gatewayLayer) GetBucketInfo(ctx context.Context, bucketName string) (bucketInfo minio.BucketInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	project, err := layer.openProject(ctx, getAccessGrant(ctx))
	if err != nil {
		return minio.BucketInfo{}, err
	}
	defer func() {
		err = errs.Combine(err, project.Close())
	}()

	bucket, err := project.StatBucket(ctx, bucketName)
	if err != nil {
		return minio.BucketInfo{}, convertError(err, bucketName, "")
	}

	return minio.BucketInfo{
		Name:    bucket.Name,
		Created: bucket.Created,
	}, nil
}

func (layer *gatewayLayer) GetObjectNInfo(ctx context.Context, bucketName, objectPath string, rangeSpec *minio.HTTPRangeSpec, header http.Header, lockType minio.LockType, opts minio.ObjectOptions) (reader *minio.GetObjectReader, err error) {
	defer mon.Task()(&ctx)(&err)

	project, err := layer.openProject(ctx, getAccessGrant(ctx))
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errs.Combine(err, project.Close())
	}()

	// TODO this should be removed and implemented on satellite side
	defer func() {
		err = checkBucketError(ctx, project, bucketName, objectPath, err)
	}()

	startOffset := int64(0)
	length := int64(-1)
	if rangeSpec != nil {
		if rangeSpec.IsSuffixLength {
			if rangeSpec.Start > 0 {
				return nil, errs.New("Unexpected range specification case")
			}
			// TODO: can we avoid this additional call?
			object, err := project.StatObject(ctx, bucketName, objectPath)
			if err != nil {
				return nil, convertError(err, bucketName, objectPath)
			}
			startOffset, length, err = rangeSpec.GetOffsetLength(object.System.ContentLength)
			if err != nil {
				return nil, convertError(err, bucketName, objectPath)
			}
		} else if rangeSpec.End < -1 {
			return nil, errs.New("Unexpected range specification case")
		} else {
			startOffset = rangeSpec.Start
			if rangeSpec.End != -1 {
				length = rangeSpec.End - rangeSpec.Start + 1
			}
		}
	}

	download, err := project.DownloadObject(ctx, bucketName, objectPath, &uplink.DownloadOptions{
		Offset: startOffset,
		Length: length,
	})
	if err != nil {
		return nil, convertError(err, bucketName, objectPath)
	}

	object := download.Info()
	if startOffset < 0 || length < -1 {
		return nil, errs.Combine(
			minio.InvalidRange{
				OffsetBegin:  startOffset,
				OffsetEnd:    startOffset + length - 1,
				ResourceSize: object.System.ContentLength,
			},
			download.Close(),
		)
	}

	objectInfo := minioObjectInfo(bucketName, "", object)
	downloadCloser := func() { _ = download.Close() }

	return minio.NewGetObjectReaderFromReader(download, objectInfo, opts, downloadCloser)
}

func (layer *gatewayLayer) GetObject(ctx context.Context, bucketName, objectPath string, startOffset int64, length int64, writer io.Writer, etag string, opts minio.ObjectOptions) (err error) {
	defer mon.Task()(&ctx)(&err)

	project, err := layer.openProject(ctx, getAccessGrant(ctx))
	if err != nil {
		return convertError(err, bucketName, objectPath)
	}
	defer func() {
		err = errs.Combine(err, project.Close())
	}()

	download, err := project.DownloadObject(ctx, bucketName, objectPath, &uplink.DownloadOptions{
		Offset: startOffset,
		Length: length,
	})
	if err != nil {
		// TODO this should be removed and implemented on satellite side
		err = checkBucketError(ctx, project, bucketName, objectPath, err)
		return convertError(err, bucketName, objectPath)
	}
	defer func() { err = errs.Combine(err, download.Close()) }()

	object := download.Info()
	if startOffset < 0 || length < -1 {
		return minio.InvalidRange{
			OffsetBegin:  startOffset,
			OffsetEnd:    startOffset + length,
			ResourceSize: object.System.ContentLength,
		}
	}

	_, err = io.Copy(writer, download)

	return convertError(err, bucketName, objectPath)
}

func (layer *gatewayLayer) GetObjectInfo(ctx context.Context, bucketName, objectPath string, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	project, err := layer.openProject(ctx, getAccessGrant(ctx))
	if err != nil {
		return minio.ObjectInfo{}, err
	}
	defer func() {
		err = errs.Combine(err, project.Close())
	}()

	object, err := project.StatObject(ctx, bucketName, objectPath)
	if err != nil {
		// TODO this should be removed and implemented on satellite side
		err = checkBucketError(ctx, project, bucketName, objectPath, err)
		return minio.ObjectInfo{}, convertError(err, bucketName, objectPath)
	}

	return minioObjectInfo(bucketName, "", object), nil
}

func (layer *gatewayLayer) ListBuckets(ctx context.Context) (items []minio.BucketInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	project, err := layer.openProject(ctx, getAccessGrant(ctx))
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errs.Combine(err, project.Close())
	}()

	buckets := project.ListBuckets(ctx, nil)
	for buckets.Next() {
		info := buckets.Item()
		items = append(items, minio.BucketInfo{
			Name:    info.Name,
			Created: info.Created,
		})
	}
	if buckets.Err() != nil {
		return nil, buckets.Err()
	}
	return items, nil
}

func (layer *gatewayLayer) ListObjects(ctx context.Context, bucketName, prefix, marker, delimiter string, maxKeys int) (result minio.ListObjectsInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	// TODO maybe this should be checked by project.ListObjects
	if bucketName == "" {
		return minio.ListObjectsInfo{}, minio.BucketNameInvalid{}
	}

	if delimiter != "" && delimiter != "/" {
		return minio.ListObjectsInfo{}, minio.UnsupportedDelimiter{Delimiter: delimiter}
	}

	project, err := layer.openProject(ctx, getAccessGrant(ctx))
	if err != nil {
		return result, err
	}
	defer func() {
		err = errs.Combine(err, project.Close())
	}()

	// TODO this should be removed and implemented on satellite side
	defer func() {
		err = checkBucketError(ctx, project, bucketName, "", err)
	}()

	recursive := delimiter == ""

	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		// N.B.: in this case, the most S3-compatible thing we could do
		// is ask the satellite to list all siblings of this prefix that
		// share the same parent encryption key, decrypt all of them,
		// then only return the ones that have this same unencrypted
		// prefix.
		// this is terrible from a performance perspective, and it turns
		// out, many of the usages of listing without a /-suffix are
		// simply to provide a sort of StatObject like feature. in fact,
		// for example, duplicity never calls list without a /-suffix
		// in a case where it expects to get back more than one result.
		// so, we could either
		// 1) return an error here, guaranteeing nothing works
		// 2) do the full S3 compatible thing, which has terrible
		//    performance for a really common case (StatObject-like
		//		functionality)
		// 3) handle strictly more of the use cases than #1 without
		//    loss of performance by turning this into a StatObject.
		// so we do #3 here. it's great!

		return listSingleObject(ctx, project, bucketName, prefix, recursive)
	}

	list := project.ListObjects(ctx, bucketName, &uplink.ListObjectsOptions{
		Prefix:    prefix,
		Cursor:    marker,
		Recursive: recursive,

		System: true,
		Custom: true,
	})

	startAfter := marker
	var objects []minio.ObjectInfo
	var prefixes []string

	limit := maxKeys
	for (limit > 0 || maxKeys == 0) && list.Next() {
		limit--
		object := list.Item()
		if object.IsPrefix {
			prefixes = append(prefixes, object.Key)
			continue
		}

		objects = append(objects, minioObjectInfo(bucketName, "", object))

		startAfter = object.Key

	}
	if list.Err() != nil {
		return result, convertError(list.Err(), bucketName, "")
	}

	more := list.Next()
	if list.Err() != nil {
		return result, convertError(list.Err(), bucketName, "")
	}

	result = minio.ListObjectsInfo{
		IsTruncated: more,
		Objects:     objects,
		Prefixes:    prefixes,
	}
	if more {
		result.NextMarker = startAfter
	}

	return result, nil
}

func listSingleObject(ctx context.Context, project *uplink.Project, bucketName, key string, recursive bool) (result minio.ListObjectsInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	var prefixes []string
	if !recursive {
		list := project.ListObjects(ctx, bucketName, &uplink.ListObjectsOptions{
			Prefix:    key + "/",
			Recursive: true,
			// Limit: 1, would be nice to set here
		})
		if list.Next() {
			prefixes = append(prefixes, key+"/")
		}
		if err := list.Err(); err != nil {
			return minio.ListObjectsInfo{}, convertError(err, bucketName, key)
		}
	}

	var objects []minio.ObjectInfo
	object, err := project.StatObject(ctx, bucketName, key)
	if err != nil {
		if !errors.Is(err, uplink.ErrObjectNotFound) {
			return minio.ListObjectsInfo{}, convertError(err, bucketName, key)
		}
	} else {
		objects = append(objects, minioObjectInfo(bucketName, "", object))
	}

	return minio.ListObjectsInfo{
		IsTruncated: false,
		Prefixes:    prefixes,
		Objects:     objects,
	}, nil
}

func (layer *gatewayLayer) ListObjectsV2(ctx context.Context, bucketName, prefix, continuationToken, delimiter string, maxKeys int, fetchOwner bool, startAfter string) (result minio.ListObjectsV2Info, err error) {
	defer mon.Task()(&ctx)(&err)

	if delimiter != "" && delimiter != "/" {
		return minio.ListObjectsV2Info{ContinuationToken: continuationToken}, minio.UnsupportedDelimiter{Delimiter: delimiter}
	}

	project, err := layer.openProject(ctx, getAccessGrant(ctx))
	if err != nil {
		return result, err
	}
	defer func() {
		err = errs.Combine(err, project.Close())
	}()

	// TODO this should be removed and implemented on satellite side
	defer func() {
		err = checkBucketError(ctx, project, bucketName, "", err)
	}()

	recursive := delimiter == ""

	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		// N.B.: in this case, the most S3-compatible thing we could do
		// is ask the satellite to list all siblings of this prefix that
		// share the same parent encryption key, decrypt all of them,
		// then only return the ones that have this same unencrypted
		// prefix.
		// this is terrible from a performance perspective, and it turns
		// out, many of the usages of listing without a /-suffix are
		// simply to provide a sort of StatObject like feature. in fact,
		// for example, duplicity never calls list without a /-suffix
		// in a case where it expects to get back more than one result.
		// so, we could either
		// 1) return an error here, guaranteeing nothing works
		// 2) do the full S3 compatible thing, which has terrible
		//    performance for a really common case (StatObject-like
		//		functionality)
		// 3) handle strictly more of the use cases than #1 without
		//    loss of performance by turning this into a StatObject.
		// so we do #3 here. it's great!

		return listSingleObjectV2(ctx, project, bucketName, prefix, recursive, fetchOwner)
	}

	var startAfterPath storj.Path
	if continuationToken != "" {
		startAfterPath = continuationToken
	}
	if startAfterPath == "" && startAfter != "" {
		startAfterPath = startAfter
	}

	var objects []minio.ObjectInfo
	var prefixes []string

	list := project.ListObjects(ctx, bucketName, &uplink.ListObjectsOptions{
		Prefix:    prefix,
		Cursor:    startAfterPath,
		Recursive: recursive,

		System: true,
		Custom: true,
	})

	limit := maxKeys
	for (limit > 0 || maxKeys == 0) && list.Next() {
		limit--
		object := list.Item()
		if object.IsPrefix {
			prefixes = append(prefixes, object.Key)
			continue
		}

		objects = append(objects, minioObjectInfo(bucketName, "", object))

		startAfter = object.Key
	}
	if list.Err() != nil {
		return result, convertError(list.Err(), bucketName, "")
	}

	more := list.Next()
	if list.Err() != nil {
		return result, convertError(list.Err(), bucketName, "")
	}

	result = minio.ListObjectsV2Info{
		IsTruncated:       more,
		ContinuationToken: startAfter,
		Objects:           objects,
		Prefixes:          prefixes,
	}
	if more {
		result.NextContinuationToken = startAfter
	}

	return result, nil
}

func listSingleObjectV2(ctx context.Context, project *uplink.Project, bucketName, key string, recursive, fetchOwner bool) (result minio.ListObjectsV2Info, err error) {
	defer mon.Task()(&ctx)(&err)

	var prefixes []string
	if !recursive {
		list := project.ListObjects(ctx, bucketName, &uplink.ListObjectsOptions{
			Prefix:    key + "/",
			Recursive: true,
			// Limit: 1, would be nice to set here
		})
		if list.Next() {
			prefixes = append(prefixes, key+"/")
		}
		if err := list.Err(); err != nil {
			return minio.ListObjectsV2Info{}, convertError(err, bucketName, key)
		}
	}

	var objects []minio.ObjectInfo
	object, err := project.StatObject(ctx, bucketName, key)
	if err != nil {
		if !errors.Is(err, uplink.ErrObjectNotFound) {
			return minio.ListObjectsV2Info{}, convertError(err, bucketName, key)
		}
	} else {
		objects = append(objects, minioObjectInfo(bucketName, "", object))
	}

	return minio.ListObjectsV2Info{
		IsTruncated: false,
		Prefixes:    prefixes,
		Objects:     objects,
	}, nil
}

func (layer *gatewayLayer) MakeBucketWithLocation(ctx context.Context, bucketName string, opts minio.BucketOptions) (err error) {
	defer mon.Task()(&ctx)(&err)

	project, err := layer.openProject(ctx, getAccessGrant(ctx))
	if err != nil {
		return convertError(err, bucketName, "")
	}
	defer func() {
		err = errs.Combine(err, project.Close())
	}()

	_, err = project.CreateBucket(ctx, bucketName)

	return convertError(err, bucketName, "")
}

func (layer *gatewayLayer) CopyObject(ctx context.Context, srcBucket, srcObject, destBucket, destObject string, srcInfo minio.ObjectInfo, srcOpts, destOpts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	// Scenario: if a client starts uploading an object and then dies, when
	// is it safe to restart uploading?
	// * with libuplink natively, it's immediately safe. the client died, so
	//   it stopped however far it got, and it can start over.
	// * with the gateway, unless we do the following line it is impossible
	//   to know when it's safe to start uploading again. it might be up to
	//   30 minutes later that it's safe! the reason is if the client goes
	//   away, the gateway keeps running, and may down the road decide the
	//   request was canceled, and so the object should get deleted.
	// So, to make clients of the gateway's behavior match libuplink, we are
	// disabling the cleanup on cancel that libuplink tries to do. we may
	// want to consider disabling this for libuplink entirely.
	// The following line currently only impacts UploadObject calls.
	ctx = streams.DisableDeleteOnCancel(ctx)

	// TODO: We want to return Not Implemented until we implement server-side copy
	return minio.ObjectInfo{}, minio.NotImplemented{API: "CopyObject"}

	// if srcObject == "" {
	// 	return minio.ObjectInfo{}, minio.ObjectNameInvalid{Bucket: srcBucket}
	// }
	// if destObject == "" {
	// 	return minio.ObjectInfo{}, minio.ObjectNameInvalid{Bucket: destBucket}
	// }

	// project, err := layer.openProject(ctx, getAccessGrant(ctx))
	// if err != nil {
	// 	return minio.ObjectInfo{}, err
	// }

	// // TODO this should be removed and implemented on satellite side
	// _, err = project.StatBucket(ctx, srcBucket)
	// if err != nil {
	// 	return minio.ObjectInfo{}, convertError(err, srcBucket, "")
	// }

	// // TODO this should be removed and implemented on satellite side
	// if srcBucket != destBucket {
	// 	_, err = project.StatBucket(ctx, destBucket)
	// 	if err != nil {
	// 		return minio.ObjectInfo{}, convertError(err, destBucket, "")
	// 	}
	// }

	// if srcBucket == destBucket && srcObject == destObject {
	// 	// Source and destination are the same. Do nothing, otherwise copying
	// 	// the same object over itself may destroy it, especially if it is a
	// 	// larger one.
	// 	return srcInfo, nil
	// }

	// download, err := project.DownloadObject(ctx, srcBucket, srcObject, nil)
	// if err != nil {
	// 	return minio.ObjectInfo{}, convertError(err, srcBucket, srcObject)
	// }
	// defer func() {
	// 	// TODO: this hides minio error
	// 	err = errs.Combine(err, download.Close())
	// }()

	// upload, err := project.UploadObject(ctx, destBucket, destObject, nil)
	// if err != nil {
	// 	return minio.ObjectInfo{}, convertError(err, destBucket, destObject)
	// }

	// info := download.Info()
	// err = upload.SetCustomMetadata(ctx, info.Custom)
	// if err != nil {
	// 	abortErr := upload.Abort()
	// 	err = errs.Combine(err, abortErr)
	// 	return minio.ObjectInfo{}, convertError(err, destBucket, destObject)
	// }

	// reader, err := hash.NewReader(download, info.System.ContentLength, "", "", info.System.ContentLength, true)
	// if err != nil {
	// 	abortErr := upload.Abort()
	// 	err = errs.Combine(err, abortErr)
	// 	return minio.ObjectInfo{}, convertError(err, destBucket, destObject)
	// }

	// _, err = io.Copy(upload, reader)
	// if err != nil {
	// 	abortErr := upload.Abort()
	// 	err = errs.Combine(err, abortErr)
	// 	return minio.ObjectInfo{}, convertError(err, destBucket, destObject)
	// }

	// err = upload.Commit()
	// if err != nil {
	// 	return minio.ObjectInfo{}, convertError(err, destBucket, destObject)
	// }

	// return minioObjectInfo(destBucket, hex.EncodeToString(reader.MD5Current()), upload.Info()), nil
}

func (layer *gatewayLayer) PutObject(ctx context.Context, bucketName, objectPath string, data *minio.PutObjReader, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	// Scenario: if a client starts uploading an object and then dies, when
	// is it safe to restart uploading?
	// * with libuplink natively, it's immediately safe. the client died, so
	//   it stopped however far it got, and it can start over.
	// * with the gateway, unless we do the following line it is impossible
	//   to know when it's safe to start uploading again. it might be up to
	//   30 minutes later that it's safe! the reason is if the client goes
	//   away, the gateway keeps running, and may down the road decide the
	//   request was canceled, and so the object should get deleted.
	// So, to make clients of the gateway's behavior match libuplink, we are
	// disabling the cleanup on cancel that libuplink tries to do. we may
	// want to consider disabling this for libuplink entirely.
	// The following line currently only impacts UploadObject calls.
	ctx = streams.DisableDeleteOnCancel(ctx)

	project, err := layer.openProject(ctx, getAccessGrant(ctx))
	if err != nil {
		return minio.ObjectInfo{}, err
	}
	defer func() {
		err = errs.Combine(err, project.Close())
	}()

	// TODO this should be removed and implemented on satellite side
	defer func() {
		err = checkBucketError(ctx, project, bucketName, objectPath, err)
	}()

	if data == nil {
		hashReader, err := hash.NewReader(bytes.NewReader([]byte{}), 0, "", "", 0, true)
		if err != nil {
			return minio.ObjectInfo{}, convertError(err, bucketName, objectPath)
		}
		data = minio.NewPutObjReader(hashReader, nil, nil)
	}

	upload, err := project.UploadObject(ctx, bucketName, objectPath, nil)
	if err != nil {
		return minio.ObjectInfo{}, convertError(err, bucketName, objectPath)
	}

	_, err = io.Copy(upload, data)
	if err != nil {
		abortErr := upload.Abort()
		err = errs.Combine(err, abortErr)
		return minio.ObjectInfo{}, convertError(err, bucketName, objectPath)
	}

	opts.UserDefined["s3:etag"] = hex.EncodeToString(data.MD5Current())
	err = upload.SetCustomMetadata(ctx, opts.UserDefined)
	if err != nil {
		abortErr := upload.Abort()
		err = errs.Combine(err, abortErr)
		return minio.ObjectInfo{}, convertError(err, bucketName, objectPath)
	}

	err = upload.Commit()
	if err != nil {
		return minio.ObjectInfo{}, convertError(err, bucketName, objectPath)
	}

	return minioObjectInfo(bucketName, opts.UserDefined["s3:etag"], upload.Info()), nil
}

func (layer *gatewayLayer) Shutdown(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	return layer.gateway.connectionPool.Close()
}

func (layer *gatewayLayer) StorageInfo(ctx context.Context, local bool) (minio.StorageInfo, []error) {
	info := minio.StorageInfo{}
	info.Backend.Type = minio.BackendGateway
	info.Backend.GatewayOnline = true
	return info, nil
}

func (layer *gatewayLayer) setupProject(ctx context.Context, access *uplink.Access) (_ *uplink.Project, err error) {
	defer mon.Task()(&ctx)(&err)

	config := layer.gateway.config
	config.UserAgent = getUserAgent(ctx)

	err = transport.SetConnectionPool(ctx, &config, layer.gateway.connectionPool)
	if err != nil {
		return nil, err
	}

	return config.OpenProject(ctx, access)
}

func (layer *gatewayLayer) openProject(ctx context.Context, accessKey string) (_ *uplink.Project, err error) {
	defer mon.Task()(&ctx)(&err)

	if accessKey == "" {
		return nil, errs.New("Access key is empty")
	}

	access, err := uplink.ParseAccess(accessKey)
	if err != nil {
		return nil, err
	}

	return layer.setupProject(ctx, access)
}

func (layer *gatewayLayer) openProjectMultipart(ctx context.Context, accessKey string) (_ *uplink.Project, err error) {
	defer mon.Task()(&ctx)(&err)

	access, err := uplink.ParseAccess(accessKey)
	if err != nil {
		return nil, err
	}

	nodeURL, err := storj.ParseNodeURL(access.SatelliteAddress())
	if err != nil {
		return nil, err
	}

	reqInfo := logger.GetReqInfo(ctx)
	if reqInfo == nil {
		return nil, err
	}

	if _, ok := layer.gateway.multipartSatAddrs[nodeURL.Address]; !ok {
		return nil, minio.NotImplemented{API: reqInfo.API}
	}

	return layer.setupProject(ctx, access)
}

// checkBucketError will stat the bucket if the provided error is not nil, in
// order to check if the proper error to return is really a bucket not found
// error. If the satellite has already returned this error, do not make an
// additional check.
func checkBucketError(ctx context.Context, project *uplink.Project, bucketName, object string, err error) error {
	if err != nil && !errors.Is(err, uplink.ErrBucketNotFound) {
		_, statErr := project.StatBucket(ctx, bucketName)
		if statErr != nil {
			return convertError(statErr, bucketName, object)
		}
	}
	return err
}

func convertError(err error, bucket, object string) error {
	if errors.Is(err, uplink.ErrBucketNameInvalid) {
		return minio.BucketNameInvalid{Bucket: bucket}
	}

	if errors.Is(err, uplink.ErrBucketAlreadyExists) {
		return minio.BucketAlreadyExists{Bucket: bucket}
	}

	if errors.Is(err, uplink.ErrBucketNotFound) {
		return minio.BucketNotFound{Bucket: bucket}
	}

	if errors.Is(err, uplink.ErrBucketNotEmpty) {
		return minio.BucketNotEmpty{Bucket: bucket}
	}

	if errors.Is(err, uplink.ErrObjectKeyInvalid) {
		return minio.ObjectNameInvalid{Bucket: bucket, Object: object}
	}

	if errors.Is(err, uplink.ErrObjectNotFound) {
		return minio.ObjectNotFound{Bucket: bucket, Object: object}
	}

	if errs2.IsRPC(err, rpcstatus.ResourceExhausted) {
		return minio.ProjectUsageLimit{}
	}

	return err
}

func minioObjectInfo(bucket, etag string, object *uplink.Object) minio.ObjectInfo {
	if object == nil {
		object = &uplink.Object{}
	}

	contentType := ""
	for k, v := range object.Custom {
		if strings.ToLower(k) == "content-type" {
			contentType = v
			break
		}
	}
	if etag == "" {
		etag = object.Custom["s3:etag"]
	}
	return minio.ObjectInfo{
		Bucket:      bucket,
		Name:        object.Key,
		Size:        object.System.ContentLength,
		ETag:        etag,
		ModTime:     object.System.Created,
		ContentType: contentType,
		UserDefined: object.Custom,
	}
}

func getAccessGrant(ctx context.Context) string {
	reqInfo := logger.GetReqInfo(ctx)
	if reqInfo == nil {
		return ""
	}
	return reqInfo.AccessGrant
}

func getUserAgent(ctx context.Context) string {
	userAgent := gatewayUserAgent
	reqInfo := logger.GetReqInfo(ctx)
	if reqInfo == nil {
		return userAgent
	}

	if reqInfo.UserAgent != "" {
		userAgent = reqInfo.UserAgent + " " + userAgent
	}

	return userAgent
}
