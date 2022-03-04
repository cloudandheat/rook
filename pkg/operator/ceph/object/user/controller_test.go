/*
Copyright 2018 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package objectuser to manage a rook object store.
package objectuser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/ceph/go-ceph/rgw/admin"
	"github.com/coreos/pkg/capnslog"
	"github.com/pkg/errors"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	rookclient "github.com/rook/rook/pkg/client/clientset/versioned/fake"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"github.com/rook/rook/pkg/operator/test"

	"github.com/rook/rook/pkg/clusterd"
	cephobject "github.com/rook/rook/pkg/operator/ceph/object"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	userCreateJSON = `{
	"user_id": "my-user",
	"display_name": "my-user",
	"email": "",
	"suspended": 0,
	"max_buckets": 1000,
	"subusers": [],
	"keys": [
		{
			"user": "my-user",
			"access_key": "EOE7FYCNOBZJ5VFV909G",
			"secret_key": "qmIqpWm8HxCzmynCrD6U6vKWi4hnDBndOnmxXNsV"
		}
	],
	"swift_keys": [],
	"caps": [
		{
			"type": "users",
			"perms": "*"
		}
	],
	"op_mask": "read, write, delete",
	"default_placement": "",
	"default_storage_class": "",
	"placement_tags": [],
	"bucket_quota": {
		"enabled": false,
		"check_on_raw": false,
		"max_size": -1,
		"max_size_kb": 0,
		"max_objects": -1
	},
	"user_quota": {
		"enabled": false,
		"check_on_raw": false,
		"max_size": -1,
		"max_size_kb": 0,
		"max_objects": -1
	},
	"temp_url_keys": [],
	"type": "rgw",
	"mfa_ids": []
}`
	userCapsJSON = `[{"type":"users","perm":"read"}]`
)

var (
	name             = "my-user"
	namespace        = "rook-ceph"
	store            = "my-store"
	maxbucket        = 200
	maxsizestr       = "10G"
	maxobject  int64 = 10000
)

func TestCephObjectStoreUserController(t *testing.T) {
	ctx := context.TODO()
	// Set DEBUG logging
	capnslog.SetGlobalLogLevel(capnslog.DEBUG)
	cephObjectStore := &cephv1.CephObjectStore{}
	objectUser := &cephv1.CephObjectStoreUser{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: cephv1.ObjectStoreUserSpec{
			Store: store,
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "CephObjectStoreUser",
		},
	}
	cephCluster := &cephv1.CephCluster{}

	// Objects to track in the fake client.
	object := []runtime.Object{
		objectUser,
		cephCluster,
	}

	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			if args[0] == "status" {
				return `{"fsid":"c47cac40-9bee-4d52-823b-ccd803ba5bfe","health":{"checks":{},"status":"HEALTH_ERR"},"pgmap":{"num_pgs":100,"pgs_by_state":[{"state_name":"active+clean","count":100}]}}`, nil
			}
			return "", nil
		},
	}
	clientset := test.New(t, 3)
	c := &clusterd.Context{
		Executor:      executor,
		RookClientset: rookclient.NewSimpleClientset(),
		Clientset:     clientset,
	}

	// Register operator types with the runtime scheme.
	s := scheme.Scheme
	s.AddKnownTypes(cephv1.SchemeGroupVersion, &cephv1.CephObjectStoreUser{}, &cephv1.CephCluster{}, &cephv1.CephClusterList{})

	// Create a fake client to mock API calls.
	cl := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(object...).Build()
	// Create a ReconcileObjectStoreUser object with the scheme and fake client.
	r := &ReconcileObjectStoreUser{client: cl, scheme: s, context: c, opManagerContext: ctx}

	// Mock request to simulate Reconcile() being called on an event for a
	// watched resource .
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		},
	}

	t.Run("failure because no CephCluster", func(t *testing.T) {
		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.True(t, res.Requeue)
	})

	t.Run("failure CephCluster not ready", func(t *testing.T) {
		cephCluster = &cephv1.CephCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      namespace,
				Namespace: namespace,
			},
			Status: cephv1.ClusterStatus{
				Phase: "",
				CephStatus: &cephv1.CephStatus{
					Health: "",
				},
			},
		}

		object = append(object, cephCluster)
		// Create a fake client to mock API calls.
		cl = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(object...).Build()
		// Create a ReconcileObjectStoreUser object with the scheme and fake client.
		r = &ReconcileObjectStoreUser{client: cl, scheme: s, context: c, opManagerContext: ctx}
		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.True(t, res.Requeue)
	})

	t.Run("failure CephCluster is ready but NO rgw object", func(t *testing.T) {
		// Mock clusterInfo
		secrets := map[string][]byte{
			"fsid":         []byte(name),
			"mon-secret":   []byte("monsecret"),
			"admin-secret": []byte("adminsecret"),
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "rook-ceph-mon",
				Namespace: namespace,
			},
			Data: secrets,
			Type: k8sutil.RookType,
		}
		_, err := c.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
		assert.NoError(t, err)

		// Add ready status to the CephCluster
		cephCluster.Status.Phase = k8sutil.ReadyStatus
		cephCluster.Status.CephStatus.Health = "HEALTH_OK"

		// Create a fake client to mock API calls.
		cl = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(object...).Build()

		executor = &exectest.MockExecutor{
			MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
				if args[0] == "status" {
					return `{"fsid":"c47cac40-9bee-4d52-823b-ccd803ba5bfe","health":{"checks":{},"status":"HEALTH_OK"},"pgmap":{"num_pgs":100,"pgs_by_state":[{"state_name":"active+clean","count":100}]}}`, nil
				}
				return "", nil
			},
			MockExecuteCommandWithTimeout: func(timeout time.Duration, command string, args ...string) (string, error) {
				if args[0] == "user" {
					return userCreateJSON, nil
				}
				return "", nil
			},
		}
		c.Executor = executor

		// Create a ReconcileObjectStoreUser object with the scheme and fake client.
		r = &ReconcileObjectStoreUser{client: cl, scheme: s, context: c, opManagerContext: ctx}

		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.True(t, res.Requeue)
	})

	t.Run("failure Rgw object exists but NO pod are running", func(t *testing.T) {
		cephObjectStore = &cephv1.CephObjectStore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      store,
				Namespace: namespace,
			},
			TypeMeta: metav1.TypeMeta{
				Kind: "CephObjectStore",
			},
			Spec: cephv1.ObjectStoreSpec{
				Gateway: cephv1.GatewaySpec{
					Port: 80,
				},
			},
			Status: &cephv1.ObjectStoreStatus{
				Info: map[string]string{"endpoint": "http://rook-ceph-rgw-my-store.rook-ceph:80"},
			},
		}
		s.AddKnownTypes(cephv1.SchemeGroupVersion, &cephv1.CephObjectStore{})
		s.AddKnownTypes(cephv1.SchemeGroupVersion, &cephv1.CephObjectStoreList{})
		object = append(object, cephObjectStore)

		// Create a fake client to mock API calls.
		cl = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(object...).Build()
		// Create a ReconcileObjectStoreUser object with the scheme and fake client.
		r = &ReconcileObjectStoreUser{client: cl, scheme: s, context: c, opManagerContext: ctx}

		err := r.client.Get(context.TODO(), types.NamespacedName{Name: store, Namespace: namespace}, cephObjectStore)
		assert.NoError(t, err, cephObjectStore)
		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.True(t, res.Requeue)
	})

	t.Run("success Rgw object exists and pods are running", func(t *testing.T) {
		rgwPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      "rook-ceph-rgw-my-store-a-5fd6fb4489-xv65v",
			Namespace: namespace,
			Labels:    map[string]string{k8sutil.AppAttr: appName, "rgw": "my-store"}}}

		// Get the updated object.
		// Create RGW pod
		err := r.client.Create(context.TODO(), rgwPod)
		assert.NoError(t, err)

		// Mock client
		newMultisiteAdminOpsCtxFunc = func(objContext *cephobject.Context, spec *cephv1.ObjectStoreSpec) (*cephobject.AdminOpsContext, error) {
			mockClient := &cephobject.MockClient{
				MockDo: func(req *http.Request) (*http.Response, error) {
					if (req.URL.RawQuery == "format=json&uid=my-user" && (req.Method == http.MethodGet || req.Method == http.MethodPost) && req.URL.Path == "rook-ceph-rgw-my-store.mycluster.svc/admin/user") ||
						(req.URL.RawQuery == "enabled=false&format=json&max-objects=-1&max-size=-1&quota=&quota-type=user&uid=my-user" && req.Method == http.MethodPut && req.URL.Path == "rook-ceph-rgw-my-store.mycluster.svc/admin/user") {
						return &http.Response{
							StatusCode: 200,
							Body:       ioutil.NopCloser(bytes.NewReader([]byte(userCreateJSON))),
						}, nil
					}
					return nil, fmt.Errorf("unexpected request: %q. method %q. path %q", req.URL.RawQuery, req.Method, req.URL.Path)
				},
			}

			context, err := cephobject.NewMultisiteContext(r.context, r.clusterInfo, cephObjectStore)
			assert.NoError(t, err)
			adminClient, err := admin.New("rook-ceph-rgw-my-store.mycluster.svc", "53S6B9S809NUP19IJ2K3", "1bXPegzsGClvoGAiJdHQD1uOW2sQBLAZM9j9VtXR", mockClient)
			assert.NoError(t, err)

			return &cephobject.AdminOpsContext{
				Context:               *context,
				AdminOpsUserAccessKey: "53S6B9S809NUP19IJ2K3",
				AdminOpsUserSecretKey: "1bXPegzsGClvoGAiJdHQD1uOW2sQBLAZM9j9VtXR",
				AdminOpsClient:        adminClient,
			}, nil
		}

		// Run reconcile
		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.False(t, res.Requeue)
		err = r.client.Get(context.TODO(), req.NamespacedName, objectUser)
		assert.NoError(t, err)
		assert.Equal(t, "Ready", objectUser.Status.Phase, objectUser)
	})
}

func TestBuildUpdateStatusInfo(t *testing.T) {
	cephObjectStoreUser := &cephv1.CephObjectStoreUser{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: cephv1.ObjectStoreUserSpec{
			Store: store,
		},
	}

	statusInfo := generateStatusInfo(cephObjectStoreUser)
	assert.NotEmpty(t, statusInfo["secretName"])
	assert.Equal(t, "rook-ceph-object-user-my-store-my-user", statusInfo["secretName"])
}

func TestCreateorUpdateCephUser(t *testing.T) {
	// Set DEBUG logging
	capnslog.SetGlobalLogLevel(capnslog.DEBUG)

	objectUser := &cephv1.CephObjectStoreUser{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "",
			Namespace: namespace,
		},
		Spec: cephv1.ObjectStoreUserSpec{
			Store: store,
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "CephObjectStoreUser",
		},
	}
	mockClient := &cephobject.MockClient{
		MockDo: func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "rook-ceph-rgw-my-store.mycluster.svc/admin/user" {
				return nil, fmt.Errorf("unexpected url path %q", req.URL.Path)
			}
			if req.Method == http.MethodGet {
				if req.URL.RawQuery == "format=json&uid=my-user" {
					return &http.Response{
						StatusCode: 200,
						Body:       ioutil.NopCloser(bytes.NewReader([]byte(userCreateJSON))),
					}, nil
				}
			}

			if req.Method == http.MethodPost {
				if req.URL.RawQuery == "display-name=my-user&format=json&max-buckets=1000&uid=my-user" ||
					req.URL.RawQuery == "display-name=my-user&format=json&max-buckets=200&uid=my-user" ||
					req.URL.RawQuery == "display-name=my-user&format=json&max-buckets=1000&uid=my-user&user-caps=users%3Dread%3Bbuckets%3Dread%3B" ||
					req.URL.RawQuery == "display-name=my-user&format=json&max-buckets=200&uid=my-user&user-caps=users%3Dread%3Bbuckets%3Dread%3B" {
					return &http.Response{
						StatusCode: 200,
						Body:       ioutil.NopCloser(bytes.NewReader([]byte(userCreateJSON))),
					}, nil
				}
			}

			if req.Method == http.MethodDelete {
				if req.URL.RawQuery == "caps=&format=json&uid=my-user&user-caps=users%3Dread%3Bbuckets%3Dread%3B" {
					return &http.Response{
						StatusCode: 200,
						Body:       ioutil.NopCloser(bytes.NewReader([]byte(`[]`))),
					}, nil
				}
			}
			if req.Method == http.MethodPut {
				if req.URL.RawQuery == "enabled=false&format=json&max-objects=-1&max-size=-1&quota=&quota-type=user&uid=my-user" ||
					req.URL.RawQuery == "enabled=true&format=json&max-objects=10000&max-size=-1&quota=&quota-type=user&uid=my-user" ||
					req.URL.RawQuery == "enabled=true&format=json&max-objects=-1&max-size=10000000000&quota=&quota-type=user&uid=my-user" ||
					req.URL.RawQuery == "enabled=true&format=json&max-objects=10000&max-size=10000000000&quota=&quota-type=user&uid=my-user" {
					return &http.Response{
						StatusCode: 200,
						Body:       ioutil.NopCloser(bytes.NewReader([]byte(userCreateJSON))),
					}, nil
				} else if req.URL.RawQuery == "caps=&format=json&uid=my-user&user-caps=users%3Dread%3Bbuckets%3Dread%3B" {
					return &http.Response{
						StatusCode: 200,
						Body:       ioutil.NopCloser(bytes.NewReader([]byte(userCapsJSON))),
					}, nil
				}
			}

			return nil, fmt.Errorf("unexpected request: %q. method %q. path %q", req.URL.RawQuery, req.Method, req.URL.Path)
		},
	}
	adminClient, err := admin.New("rook-ceph-rgw-my-store.mycluster.svc", "53S6B9S809NUP19IJ2K3", "1bXPegzsGClvoGAiJdHQD1uOW2sQBLAZM9j9VtXR", mockClient)
	assert.NoError(t, err)
	userConfig := generateUserConfig(objectUser)
	r := &ReconcileObjectStoreUser{
		objContext: &cephobject.AdminOpsContext{
			AdminOpsClient: adminClient,
		},
		userConfig:       &userConfig,
		opManagerContext: context.TODO(),
	}
	maxsize, err := resource.ParseQuantity(maxsizestr)
	assert.NoError(t, err)

	t.Run("user with empty name", func(t *testing.T) {
		err = r.createorUpdateCephUser(objectUser)
		assert.Error(t, err)
	})

	t.Run("user without any Quotas or Capabilities", func(t *testing.T) {
		objectUser.Name = name
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})

	t.Run("setting MaxBuckets for the user", func(t *testing.T) {
		objectUser.Spec.Quotas = &cephv1.ObjectUserQuotaSpec{MaxBuckets: &maxbucket}
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})

	t.Run("setting Capabilities for the user", func(t *testing.T) {
		objectUser.Spec.Quotas = nil
		objectUser.Spec.Capabilities = &cephv1.ObjectUserCapSpec{
			User:   "read",
			Bucket: "read",
		}
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})

	// Testing UserQuotaSpec : MaxObjects and MaxSize
	t.Run("setting MaxObjects for the user", func(t *testing.T) {
		objectUser.Spec.Capabilities = nil
		objectUser.Spec.Quotas = &cephv1.ObjectUserQuotaSpec{MaxObjects: &maxobject}
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})
	t.Run("setting MaxSize for the user", func(t *testing.T) {
		objectUser.Spec.Quotas = &cephv1.ObjectUserQuotaSpec{MaxSize: &maxsize}
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})
	t.Run("resetting MaxSize and MaxObjects for the user", func(t *testing.T) {
		objectUser.Spec.Quotas = nil
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})
	t.Run("setting both MaxSize and MaxObjects for the user", func(t *testing.T) {
		objectUser.Spec.Quotas = &cephv1.ObjectUserQuotaSpec{MaxObjects: &maxobject, MaxSize: &maxsize}
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})
	t.Run("resetting MaxSize and MaxObjects again for the user", func(t *testing.T) {
		objectUser.Spec.Quotas = nil
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})

	t.Run("setting both Quotas and Capabilities for the user", func(t *testing.T) {
		objectUser.Spec.Capabilities = &cephv1.ObjectUserCapSpec{
			User:   "read",
			Bucket: "read",
		}
		objectUser.Spec.Quotas = &cephv1.ObjectUserQuotaSpec{MaxBuckets: &maxbucket, MaxObjects: &maxobject, MaxSize: &maxsize}
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})
}

func TestCreateorUpdateSubusers(t *testing.T) {
	// Set DEBUG logging
	capnslog.SetGlobalLogLevel(capnslog.DEBUG)

	objectUser := &cephv1.CephObjectStoreUser{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "",
			Namespace: namespace,
		},
		Spec: cephv1.ObjectStoreUserSpec{
			Store: store,
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "CephObjectStoreUser",
		},
	}

	users := make(map[string]*admin.User)
	mockClient := &cephobject.MockClient{
		MockDo: func(req *http.Request) (*http.Response, error) {
			fmt.Printf("REQUEST %s %s\n", req.Method, req.URL)

			if req.URL.Path != "rook-ceph-rgw-my-store.mycluster.svc/admin/user" {
				return nil, fmt.Errorf("unexpected url path %q", req.URL.Path)
			}

			values, err := url.ParseQuery(req.URL.RawQuery)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid query")
			}

			if values.Get("format") != "json" {
				return nil, fmt.Errorf("unexpected format %q", values.Get("format"))
			}

			if req.Method == http.MethodGet {
				user, ok := users[values.Get("uid")]
				if !ok {
					return &http.Response{
						StatusCode: 404,
						Body:       ioutil.NopCloser(bytes.NewReader([]byte(`{"Code":"NoSuchUser","RequestId":"tx0000000000000000005a9-00608957a2-10496-my-store","HostId":"10496-my-store-my-store"}`))),
					}, nil
				}
				resp, err := json.Marshal(user)
				fmt.Printf("GET RESPONSE: %s\n", resp)
				if err != nil {
					return nil, err
				}

				return &http.Response{
					StatusCode: 201,
					Body:       ioutil.NopCloser(bytes.NewReader(resp)),
				}, nil
			}

			if req.Method == http.MethodPost {
				user, ok := users[values.Get("uid")]
				if values.Has("subuser") {
					if !ok {
						return nil, fmt.Errorf("trying to modify a subuser for non-existant user %q", values.Get("uid"))
					}

					for i, subuser := range user.Subusers {
						if subuser.Name == values.Get("subuser") {
							user.Subusers[i].Access = admin.SubuserAccess(values.Get("access"))
						}
						return &http.Response{
							StatusCode: 200,
							Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
						}, nil
					}

					return nil, fmt.Errorf("trying to modify non-existant subuser %q", values.Get("subuser"))
				}

				if !ok {
					return nil, fmt.Errorf("trying to modify existing user %q", values.Get("uid"))
				}

				u := users[values.Get("uid")]

				if values.Has("max-buckets") {
					var maxBuckets int
					fmt.Scanf(values.Get("max-buckets"), "%d", &maxBuckets)
					u.MaxBuckets = &maxBuckets
				}

				if values.Has("display-name") {
					u.DisplayName = values.Get("DisplayName")
				}

				resp, err := json.Marshal(u)
				if err != nil {
					return nil, err
				}

				return &http.Response{
					StatusCode: 201,
					Body:       ioutil.NopCloser(bytes.NewReader(resp)),
				}, nil
			}

			if req.Method == http.MethodDelete {
				user, ok := users[values.Get("uid")]
				if values.Has("subuser") {
					if !ok {
						return nil, fmt.Errorf("trying to create a subuser for non-existant user %q", values.Get("uid"))
					}

					newSubusers := make([]admin.SubuserSpec, 0)
					deleted := false
					for _, subuser := range user.Subusers {
						if subuser.Name == values.Get("subuser") {
							deleted = true
						} else {
							newSubusers = append(newSubusers, subuser)
						}
					}

					if !deleted {
						return nil, fmt.Errorf("trying to delete non-existant subuser %q", values.Get("subuser"))
					}

					user.Subusers = newSubusers

					return &http.Response{
						StatusCode: 201,
						Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
					}, nil
				}

				if !ok {
					return nil, fmt.Errorf("trying to delete non-existant user %q", values.Get("uid"))
				}
				delete(users, values.Get("uid"))
				return &http.Response{
					StatusCode: 200,
					Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
				}, nil
			}
			if req.Method == http.MethodPut {
				user, ok := users[values.Get("uid")]
				if values.Has("subuser") {
					if !ok {
						return nil, fmt.Errorf("trying to create a subuser for non-existant user %q", values.Get("uid"))
					}

					for _, subuser := range user.Subusers {
						if subuser.Name == values.Get("subuser") {
							return nil, fmt.Errorf("trying to create existing subuser %q", values.Get("subuser"))
						}
					}

					user.Subusers = append(user.Subusers, admin.SubuserSpec{
						Name:   values.Get("subuser"),
						Access: admin.SubuserAccess(values.Get("access")), // TODO: map this to the reply values
					})

					return &http.Response{
						StatusCode: 201,
						Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
					}, nil
				}

				if values.Has("quota") {
					return &http.Response{
						StatusCode: 200,
						Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
					}, nil
				}

				if ok {
					return nil, fmt.Errorf("trying to create existing user %q", values.Get("uid"))
				}

				maxBuckets := -1
				if values.Has("max-buckets") {
					fmt.Scanf(values.Get("max-buckets"), "%d", &maxBuckets)
				}

				u := &admin.User{
					ID: values.Get("uid"),
					Keys: []admin.UserKeySpec{
						{User: values.Get("uid"), AccessKey: "access_key", SecretKey: "secret_key"},
					},
					MaxBuckets:  &maxBuckets,
					DisplayName: values.Get("display-name"),
				}
				users[values.Get("uid")] = u

				resp, err := json.Marshal(u)
				if err != nil {
					return nil, err
				}

				return &http.Response{
					StatusCode: 201,
					Body:       ioutil.NopCloser(bytes.NewReader(resp)),
				}, nil
			}
			return nil, fmt.Errorf("unexpected request: %q. method %q. path %q", req.URL.RawQuery, req.Method, req.URL.Path)
		},
	}

	adminClient, err := admin.New("rook-ceph-rgw-my-store.mycluster.svc", "53S6B9S809NUP19IJ2K3", "1bXPegzsGClvoGAiJdHQD1uOW2sQBLAZM9j9VtXR", mockClient)
	assert.NoError(t, err)
	userConfig := generateUserConfig(objectUser)

	r := &ReconcileObjectStoreUser{
		objContext: &cephobject.AdminOpsContext{
			AdminOpsClient: adminClient,
		},
		userConfig:       &userConfig,
		opManagerContext: context.TODO(),
	}

	t.Run("user without any Quotas or Capabilities", func(t *testing.T) {
		objectUser.Name = name
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})

	t.Run("add a subuser", func(t *testing.T) {
		objectUser.Spec.Subusers = []cephv1.SubuserSpec{
			{
				Name:   "swift",
				Access: cephv1.AccessSpecRead,
			},
		}
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})

	t.Run("modify a subuser", func(t *testing.T) {
		objectUser.Spec.Subusers = []cephv1.SubuserSpec{
			{
				Name:   "swift",
				Access: cephv1.AccessSpecWrite,
			},
		}
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})

	t.Run("replace the subuser", func(t *testing.T) {
		objectUser.Spec.Subusers = []cephv1.SubuserSpec{
			{
				Name:   "swift-wo",
				Access: cephv1.AccessSpecWrite,
			},
		}
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})

	t.Run("remove all subusers", func(t *testing.T) {
		objectUser.Spec.Subusers = []cephv1.SubuserSpec{}
		userConfig = generateUserConfig(objectUser)
		r.userConfig = &userConfig
		err = r.createorUpdateCephUser(objectUser)
		assert.NoError(t, err)
	})
}
