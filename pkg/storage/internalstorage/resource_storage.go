package internalstorage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	genericstorage "k8s.io/apiserver/pkg/storage"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/tracing"

	internal "github.com/clusterpedia-io/api/clusterpedia"
	"github.com/clusterpedia-io/clusterpedia/pkg/runtime/scheme"
	"github.com/clusterpedia-io/clusterpedia/pkg/storage"
)

type ResourceStorage struct {
	groupResource schema.GroupResource

	db     *gorm.DB
	config storage.ResourceStorageConfig
}

func (s *ResourceStorage) GetStorageConfig() *storage.ResourceStorageConfig {
	config := s.config
	return &config
}

func (s *ResourceStorage) gvrKeyMap() map[string]interface{} {
	return map[string]interface{}{
		"group":    s.config.StorageResource.Group,
		"version":  s.config.StorageResource.Version,
		"resource": s.config.StorageResource.Resource,
	}
}

func (s *ResourceStorage) resourceKeyMap(cluster, namespace, name string) map[string]interface{} {
	return map[string]interface{}{
		"cluster":   cluster,
		"group":     s.config.StorageResource.Group,
		"version":   s.config.StorageResource.Version,
		"resource":  s.config.StorageResource.Resource,
		"namespace": namespace,
		"name":      name,
	}
}

func (s *ResourceStorage) Create(ctx context.Context, cluster string, obj runtime.Object) error {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Kind == "" {
		return fmt.Errorf("%s: kind is required", gvk)
	}

	metaobj, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	var ownerUID types.UID
	if owner := metav1.GetControllerOfNoCopy(metaobj); owner != nil {
		ownerUID = owner.UID
	}

	var buffer bytes.Buffer
	if err := s.config.Codec.Encode(obj, &buffer); err != nil {
		return err
	}

	resource := Resource{
		Cluster:         cluster,
		OwnerUID:        ownerUID,
		UID:             metaobj.GetUID(),
		Name:            metaobj.GetName(),
		Namespace:       metaobj.GetNamespace(),
		Group:           s.config.StorageResource.Group,
		Resource:        s.config.StorageResource.Resource,
		Version:         s.config.StorageResource.Version,
		Kind:            gvk.Kind,
		ResourceVersion: metaobj.GetResourceVersion(),
		Object:          buffer.Bytes(),
		CreatedAt:       metaobj.GetCreationTimestamp().Time,
	}
	if deletedAt := metaobj.GetDeletionTimestamp(); deletedAt != nil {
		resource.DeletedAt = sql.NullTime{Time: deletedAt.Time, Valid: true}
	}

	result := s.db.WithContext(ctx).Create(&resource)
	return InterpretResourceDBError(cluster, metaobj.GetName(), result.Error)
}

func (s *ResourceStorage) Update(ctx context.Context, cluster string, obj runtime.Object) error {
	metaobj, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	var buffer bytes.Buffer
	if err := s.config.Codec.Encode(obj, &buffer); err != nil {
		return err
	}

	var ownerUID types.UID
	if owner := metav1.GetControllerOfNoCopy(metaobj); owner != nil {
		ownerUID = owner.UID
	}

	// The uid may not be the same for resources with the same namespace/name
	// in the same cluster at different times.
	updatedResource := map[string]interface{}{
		"owner_uid":        ownerUID,
		"uid":              metaobj.GetUID(),
		"resource_version": metaobj.GetResourceVersion(),
		"object":           datatypes.JSON(buffer.Bytes()),
		"created_at":       metaobj.GetCreationTimestamp().Time,
	}
	if deletedAt := metaobj.GetDeletionTimestamp(); deletedAt != nil {
		updatedResource["deleted_at"] = sql.NullTime{Time: deletedAt.Time, Valid: true}
	}

	result := s.db.WithContext(ctx).Model(&Resource{}).
		Where(s.resourceKeyMap(cluster, metaobj.GetNamespace(), metaobj.GetName())).
		Updates(updatedResource)
	return InterpretResourceDBError(cluster, metaobj.GetName(), result.Error)
}

func (s *ResourceStorage) ConvertDeletedObject(obj interface{}) (runtime.Object, error) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return nil, err
	}

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, err
	}

	// Since it is not necessary to save the complete deleted object to the queue,
	// we convert the object to `PartialObjectMetadata`
	return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}, nil
}

func (s *ResourceStorage) deleteObject(cluster, namespace, name string) *gorm.DB {
	return s.db.Model(&Resource{}).Where(s.resourceKeyMap(cluster, namespace, name)).Delete(&Resource{})
}

func (s *ResourceStorage) Delete(ctx context.Context, cluster string, obj runtime.Object) error {
	metaobj, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	if result := s.deleteObject(cluster, metaobj.GetNamespace(), metaobj.GetName()); result.Error != nil {
		return InterpretResourceDBError(cluster, metaobj.GetName(), result.Error)
	}
	return nil
}

func (s *ResourceStorage) genGetObjectQuery(ctx context.Context, cluster, namespace, name string) *gorm.DB {
	return s.db.WithContext(ctx).Model(&Resource{}).Select("object").Where(s.resourceKeyMap(cluster, namespace, name))
}

func (s *ResourceStorage) Get(ctx context.Context, cluster, namespace, name string, into runtime.Object) error {
	ctx, span := tracing.Start(ctx, "Get from internalstorage",
		attribute.String("storage resource", s.config.StorageResource.String()),
		attribute.String("target type", fmt.Sprintf("%T", into)),
	)

	var objects [][]byte
	if result := s.genGetObjectQuery(ctx, cluster, namespace, name).First(&objects); result.Error != nil {
		return InterpretResourceDBError(cluster, namespace+"/"+name, result.Error)
	}

	span.AddEvent("About to decode object")
	obj, _, err := s.config.Codec.Decode(objects[0], nil, into)
	if err != nil {
		return err
	}
	if obj != into {
		return fmt.Errorf("failed to decode resource, into is %T", into)
	}
	return nil
}

func (s *ResourceStorage) genListObjectsQuery(ctx context.Context, opts *internal.ListOptions) (int64, *int64, *gorm.DB, ObjectList, error) {
	var result ObjectList
	switch {
	case opts.OnlyMetadata && opts.InjectEvents:
		result = &ResourceMetadataWithEventsList{}
	case opts.OnlyMetadata:
		result = &ResourceMetadataList{}
	case opts.InjectEvents:
		result = &BytesWithEventsList{}
	default:
		result = &BytesList{}
	}

	db := s.db.WithContext(ctx)
	query := db.Model(&Resource{}).Where(s.gvrKeyMap())
	offset, amount, query, err := applyListOptionsToResourceQuery(db, query, opts)
	return offset, amount, query, result, err
}

func (s *ResourceStorage) List(ctx context.Context, listObject runtime.Object, opts *internal.ListOptions) error {
	ctx, span := tracing.Start(ctx, "List from internalstorage",
		attribute.String("storage resource", s.config.StorageResource.String()),
		attribute.String("target type", fmt.Sprintf("%T", listObject)),
	)
	defer span.End(500 * time.Millisecond)

	offset, amount, query, result, err := s.genListObjectsQuery(ctx, opts)
	if err != nil {
		return err
	}

	if err := result.From(query); err != nil {
		return InterpretDBError(s.groupResource.String(), err)
	}
	objects := result.Items()

	list, err := meta.ListAccessor(listObject)
	if err != nil {
		return err
	}

	if opts.WithContinue != nil && *opts.WithContinue {
		if int64(len(objects)) == opts.Limit {
			list.SetContinue(strconv.FormatInt(offset+opts.Limit, 10))
		}
	}

	if amount != nil {
		// When offset is too large, the data in the response is empty and the remaining count is negative.
		// This ensures that `amount = offset + len(objects) + remain`
		remain := *amount - offset - int64(len(objects))
		list.SetRemainingItemCount(&remain)
	}

	if len(objects) == 0 {
		return nil
	}

	span.AddEvent("About to convert objects", attribute.Int("count", len(objects)))

	if unstructuredList, ok := listObject.(*unstructured.UnstructuredList); ok {
		unstructuredList.Items = make([]unstructured.Unstructured, 0, len(objects))
		for _, object := range objects {
			uObj := &unstructured.Unstructured{}
			obj, err := object.ConvertTo(s.config.Codec, uObj)
			if err != nil {
				return err
			}

			uObj, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return genericstorage.NewInternalError("the converted object is not *unstructured.Unstructured")
			}

			if uObj.GroupVersionKind().Empty() {
				if version := unstructuredList.GetAPIVersion(); version != "" {
					// set to the same APIVersion as listObject
					uObj.SetAPIVersion(version)
				}
				if rt := object.GetResourceType(); !rt.Empty() {
					uObj.SetKind(rt.Kind)
				}
			}
			unstructuredList.Items = append(unstructuredList.Items, *uObj)
		}
		return nil
	}

	listPtr, err := meta.GetItemsPtr(listObject)
	if err != nil {
		return err
	}

	v, err := conversion.EnforcePtr(listPtr)
	if err != nil || v.Kind() != reflect.Slice {
		return fmt.Errorf("need ptr to slice: %v", err)
	}

	slice := reflect.MakeSlice(v.Type(), len(objects), len(objects))
	expected := reflect.New(v.Type().Elem()).Interface().(runtime.Object)
	for i, object := range objects {
		obj, err := object.ConvertTo(s.config.Codec, expected.DeepCopyObject())
		if err != nil {
			return err
		}
		if events := object.GetEvents(); events != nil {
			if m, err := meta.Accessor(obj); err == nil {
				annos := m.GetAnnotations()
				if annos == nil {
					annos = make(map[string]string, 1)
				}
				data, err := json.Marshal(events)
				if err == nil {
					annos[internal.ShadowAnnotationEvents] = string(data)
					m.SetAnnotations(annos)
				}
			}
		}
		slice.Index(i).Set(reflect.ValueOf(obj).Elem())
	}
	v.Set(slice)
	return nil
}

func (s *ResourceStorage) Watch(_ context.Context, _ *internal.ListOptions) (watch.Interface, error) {
	return nil, apierrors.NewMethodNotSupported(s.groupResource, "watch")
}

var codec = scheme.LegacyResourceCodecs.LegacyCodec(corev1.SchemeGroupVersion)

func (s *ResourceStorage) RecordEvent(ctx context.Context, cluster string, event *corev1.Event) error {
	if event.InvolvedObject.UID == "" {
		return errors.New("invalid event: involedObject.UID is empty")
	}

	var buffer bytes.Buffer
	if err := codec.Encode(event, &buffer); err != nil {
		return err
	}
	key, _ := cache.MetaNamespaceKeyFunc(event)

	if err := s.db.WithContext(ctx).Model(&Resource{}).Where(
		map[string]interface{}{"cluster": cluster, "uid": event.InvolvedObject.UID},
	).UpdateColumns(map[string]interface{}{
		"events":                  JSONUpdate("events", string(event.UID), buffer.Bytes()),
		"event_resource_versions": JSONUpdate("event_resource_versions", key, []byte(event.ResourceVersion)),
	}).Error; err != nil {
		return InterpretResourceDBError(cluster, "", err)
	}
	return nil
}

func (s *ResourceStorage) GetResourceEvents(ctx context.Context, cluster, namespace, name string) ([]*corev1.Event, error) {
	var data []EventsBytes

	result := s.db.WithContext(ctx).Model(&Resource{}).Select("events").Where(s.resourceKeyMap(cluster, namespace, name)).First(&data)
	if result.Error != nil {
		return nil, InterpretResourceDBError(cluster, namespace+"/"+name, result.Error)
	}
	if len(data) == 0 {
		return nil, nil
	}
	return data[0].Decode()
}

func applyListOptionsToResourceQuery(db *gorm.DB, query *gorm.DB, opts *internal.ListOptions) (int64, *int64, *gorm.DB, error) {
	applyFn := func(query *gorm.DB, opts *internal.ListOptions) (*gorm.DB, error) {
		query, err := applyOwnerToResourceQuery(db, query, opts)
		if err != nil {
			return nil, err
		}

		return query, nil
	}

	return applyListOptionsToQuery(query, opts, applyFn)
}

func applyOwnerToResourceQuery(db *gorm.DB, query *gorm.DB, opts *internal.ListOptions) (*gorm.DB, error) {
	var ownerQuery interface{}
	switch {
	case len(opts.ClusterNames) != 1:
		return query, nil

	case opts.OwnerUID != "":
		ownerQuery = buildOwnerQueryByUID(db, opts.ClusterNames[0], opts.OwnerUID, opts.OwnerSeniority)

	case opts.OwnerName != "":
		var ownerNamespaces []string
		if len(opts.Namespaces) != 0 {
			// match namespaced and clustered owner resources
			ownerNamespaces = append(opts.Namespaces, "")
		}
		ownerQuery = buildOwnerQueryByName(db, opts.ClusterNames[0], ownerNamespaces, opts.OwnerGroupResource, opts.OwnerName, opts.OwnerSeniority)

	default:
		return query, nil
	}

	if _, ok := ownerQuery.(string); ok {
		query = query.Where("owner_uid = ?", ownerQuery)
	} else {
		query = query.Where("owner_uid IN (?)", ownerQuery)
	}
	return query, nil
}
