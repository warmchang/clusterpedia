package internalstorage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormschema "gorm.io/gorm/schema"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type Object interface {
	GetResourceType() ResourceType
	ConvertToUnstructured() (*unstructured.Unstructured, error)
	ConvertTo(codec runtime.Codec, object runtime.Object) (runtime.Object, error)
	GetEvents() []*corev1.Event
}

type ObjectList interface {
	From(db *gorm.DB) error
	Items() []Object
}

type ResourceType struct {
	Group    string
	Version  string
	Resource string
	Kind     string
}

func (rt ResourceType) Empty() bool {
	return rt == ResourceType{}
}

func (rt ResourceType) GroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    rt.Group,
		Version:  rt.Version,
		Resource: rt.Resource,
	}
}

type JSONMap datatypes.JSONMap

func (m JSONMap) Value() (driver.Value, error) {
	return datatypes.JSONMap(m).Value()
}

func (m *JSONMap) Scan(val interface{}) error {
	return (*datatypes.JSONMap)(m).Scan(val)
}

func (m JSONMap) MarshalJSON() ([]byte, error) {
	return (datatypes.JSONMap)(m).MarshalJSON()
}

func (m *JSONMap) UnmarshalJSON(b []byte) error {
	return (*datatypes.JSONMap)(m).UnmarshalJSON(b)
}

func (m JSONMap) GormDataType() string {
	return datatypes.JSONMap(m).GormDataType()
}

func (m JSONMap) GormDBDataType(db *gorm.DB, field *gormschema.Field) string {
	return datatypes.JSONMap(m).GormDBDataType(db, field)
}

func (m JSONMap) GormValue(ctx context.Context, db *gorm.DB) clause.Expr {
	if m == nil {
		// Ensure that all empty values are stored as NULL,
		// rather than having both the string 'null' and actual NULL values
		return gorm.Expr("NULL")
	}
	return datatypes.JSONMap(m).GormValue(ctx, db)
}

type Resource struct {
	ID uint `gorm:"primaryKey"`

	Group    string `gorm:"size:63;not null;uniqueIndex:uni_group_version_resource_cluster_namespace_name;index:idx_group_version_resource_namespace_name;index:idx_group_version_resource_name"`
	Version  string `gorm:"size:15;not null;uniqueIndex:uni_group_version_resource_cluster_namespace_name;index:idx_group_version_resource_namespace_name;index:idx_group_version_resource_name"`
	Resource string `gorm:"size:63;not null;uniqueIndex:uni_group_version_resource_cluster_namespace_name;index:idx_group_version_resource_namespace_name;index:idx_group_version_resource_name"`
	Kind     string `gorm:"size:63;not null"`

	Cluster         string    `gorm:"size:253;not null;uniqueIndex:uni_group_version_resource_cluster_namespace_name,length:100;index:idx_cluster"`
	Namespace       string    `gorm:"size:253;not null;uniqueIndex:uni_group_version_resource_cluster_namespace_name,length:50;index:idx_group_version_resource_namespace_name"`
	Name            string    `gorm:"size:253;not null;uniqueIndex:uni_group_version_resource_cluster_namespace_name,length:100;index:idx_group_version_resource_namespace_name;index:idx_group_version_resource_name"`
	OwnerUID        types.UID `gorm:"column:owner_uid;size:36;not null;default:''"`
	UID             types.UID `gorm:"size:36;not null"`
	ResourceVersion string    `gorm:"size:30;not null"`

	Object datatypes.JSON `gorm:"not null"`

	// Since MySQL doesn't allow setting default values for JSON fields, we can only avoid using NOT NULL and DEFAULT.
	Events                JSONMap
	EventResourceVersions JSONMap

	CreatedAt time.Time `gorm:"not null"`
	SyncedAt  time.Time `gorm:"not null;autoUpdateTime"`
	DeletedAt sql.NullTime
}

func (res Resource) GroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    res.Group,
		Version:  res.Version,
		Resource: res.Resource,
	}
}

func (res Resource) GetResourceType() ResourceType {
	return ResourceType{
		Group:    res.Group,
		Version:  res.Version,
		Resource: res.Resource,
		Kind:     res.Kind,
	}
}

func (res Resource) ConvertToUnstructured() (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(res.Object, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func (res Resource) ConvertTo(codec runtime.Codec, object runtime.Object) (runtime.Object, error) {
	obj, _, err := codec.Decode(res.Object, nil, object)
	return obj, err
}

func (res Resource) GetEvents() []*corev1.Event {
	panic("no implemented")
}

type ResourceMetadata struct {
	ResourceType `gorm:"embedded"`

	Metadata datatypes.JSON
}

func (data ResourceMetadata) ConvertToUnstructured() (*unstructured.Unstructured, error) {
	metadata := map[string]interface{}{}
	if err := json.Unmarshal(data.Metadata, &metadata); err != nil {
		return nil, err
	}

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": schema.GroupVersion{Group: data.Group, Version: data.Version}.String(),
			"kind":       data.Kind,
			"metadata":   metadata,
		},
	}, nil
}

func (data ResourceMetadata) ConvertTo(codec runtime.Codec, object runtime.Object) (runtime.Object, error) {
	if uObj, ok := object.(*unstructured.Unstructured); ok {
		if uObj.Object == nil {
			uObj.Object = make(map[string]interface{}, 1)
		}

		metadata := map[string]interface{}{}
		if err := json.Unmarshal(data.Metadata, &metadata); err != nil {
			return nil, err
		}

		// There may be version conversions in the codec,
		// so we cannot use data.ResourceType to override `APIVersion` and `Kind`.
		uObj.Object["metadata"] = metadata
		return uObj, nil
	}

	metadata := metav1.ObjectMeta{}
	if err := json.Unmarshal(data.Metadata, &metadata); err != nil {
		return nil, err
	}
	v := reflect.ValueOf(object)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return nil, errors.New("object is nil or not pointer")
	}

	// There may be version conversions in the codec,
	// so we cannot use data.ResourceType to override `APIVersion` and `Kind`.
	v.Elem().FieldByName("ObjectMeta").Set(reflect.ValueOf(metadata))
	return object, nil
}

func (data ResourceMetadata) GetResourceType() ResourceType {
	return data.ResourceType
}

func (data ResourceMetadata) GetEvents() []*corev1.Event {
	return nil
}

type Bytes datatypes.JSON

func (bytes *Bytes) Scan(data any) error {
	return (*datatypes.JSON)(bytes).Scan(data)
}

func (bytes Bytes) Value() (driver.Value, error) {
	return (datatypes.JSON)(bytes).Value()
}

func (bytes Bytes) ConvertToUnstructured() (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(bytes, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func (bytes Bytes) ConvertTo(codec runtime.Codec, object runtime.Object) (runtime.Object, error) {
	obj, _, err := codec.Decode(bytes, nil, object)
	return obj, err
}

func (bytes Bytes) GetResourceType() ResourceType {
	return ResourceType{}
}

func (bytes Bytes) GetEvents() []*corev1.Event {
	return nil
}

type ResourceList []Resource

func (list *ResourceList) From(db *gorm.DB) error {
	resources := []Resource{}
	if result := db.Find(&resources); result.Error != nil {
		return result.Error
	}
	*list = resources
	return nil
}

func (list ResourceList) Items() []Object {
	objects := make([]Object, 0, len(list))
	for _, object := range list {
		objects = append(objects, object)
	}
	return objects
}

type ResourceMetadataList []ResourceMetadata

func (list *ResourceMetadataList) From(db *gorm.DB) error {
	switch db.Dialector.Name() {
	case "sqlite", "sqlite3", "mysql":
		db = db.Select("`group`, version, resource, kind, object->>'$.metadata' as metadata")
	case "postgres":
		db = db.Select(`"group", version, resource, kind, object->>'metadata' as metadata`)
	default:
		panic("storage: only support sqlite3, mysql or postgres")
	}
	metadatas := []ResourceMetadata{}
	if result := db.Find(&metadatas); result.Error != nil {
		return result.Error
	}
	*list = metadatas
	return nil
}

func (list ResourceMetadataList) Items() []Object {
	objects := make([]Object, 0, len(list))
	for _, object := range list {
		objects = append(objects, object)
	}
	return objects
}

type BytesList []Bytes

func (list *BytesList) From(db *gorm.DB) error {
	if result := db.Select("object").Find(list); result.Error != nil {
		return result.Error
	}
	return nil
}

func (list BytesList) Items() []Object {
	objects := make([]Object, 0, len(list))
	for _, object := range list {
		objects = append(objects, object)
	}
	return objects
}

type EventsBytes Bytes

func (bytes *EventsBytes) Scan(data any) error {
	return (*datatypes.JSON)(bytes).Scan(data)
}

func (bytes EventsBytes) Value() (driver.Value, error) {
	return (datatypes.JSON)(bytes).Value()
}

func (bytes EventsBytes) Decode() ([]*corev1.Event, error) {
	var objects map[string]json.RawMessage
	if err := json.Unmarshal(bytes, &objects); err != nil {
		return nil, err
	}

	events := make([]*corev1.Event, 0, len(objects))
	for _, obj := range objects {
		event := &corev1.Event{}
		if _, _, err := codec.Decode([]byte(obj), nil, event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

type ResourceMetadataWithEvents struct {
	ResourceMetadata `gorm:"embedded"`

	Events EventsBytes
}

func (bytes ResourceMetadataWithEvents) ConvertToUnstructured() (*unstructured.Unstructured, error) {
	return bytes.ResourceMetadata.ConvertToUnstructured()
}

func (bytes ResourceMetadataWithEvents) ConvertTo(codec runtime.Codec, object runtime.Object) (runtime.Object, error) {
	return bytes.ResourceMetadata.ConvertTo(codec, object)
}

func (bytes ResourceMetadataWithEvents) GetResourceType() ResourceType {
	return bytes.ResourceMetadata.GetResourceType()
}

func (bytes ResourceMetadataWithEvents) GetEvents() []*corev1.Event {
	events, _ := bytes.Events.Decode()
	return events
}

type ResourceMetadataWithEventsList []ResourceMetadata

func (list *ResourceMetadataWithEventsList) From(db *gorm.DB) error {
	switch db.Dialector.Name() {
	case "sqlite", "sqlite3", "mysql":
		db = db.Select("`group`, version, resource, kind, object->>'$.metadata' as metadata, events")
	case "postgres":
		db = db.Select(`"group", version, resource, kind, object->>'metadata' as metadata, events`)
	default:
		panic("storage: only support sqlite3, mysql or postgres")
	}
	metadatas := []ResourceMetadata{}
	if result := db.Find(&metadatas); result.Error != nil {
		return result.Error
	}
	*list = metadatas
	return nil
}

func (list ResourceMetadataWithEventsList) Items() []Object {
	objects := make([]Object, 0, len(list))
	for _, object := range list {
		objects = append(objects, object)
	}
	return objects
}

type BytesWithEvents struct {
	Object Bytes
	Events EventsBytes
}

func (bytes BytesWithEvents) ConvertToUnstructured() (*unstructured.Unstructured, error) {
	return bytes.Object.ConvertToUnstructured()
}

func (bytes BytesWithEvents) ConvertTo(codec runtime.Codec, object runtime.Object) (runtime.Object, error) {
	obj, _, err := codec.Decode(bytes.Object, nil, object)
	return obj, err
}

func (bytes BytesWithEvents) GetResourceType() ResourceType {
	return bytes.Object.GetResourceType()
}

func (bytes BytesWithEvents) GetEvents() []*corev1.Event {
	events, _ := bytes.Events.Decode()
	return events
}

type BytesWithEventsList []BytesWithEvents

func (list *BytesWithEventsList) From(db *gorm.DB) error {
	if result := db.Select("object", "events").Find(list); result.Error != nil {
		return result.Error
	}
	return nil
}

func (list BytesWithEventsList) Items() []Object {
	objects := make([]Object, 0, len(list))
	for _, object := range list {
		objects = append(objects, object)
	}
	return objects
}
