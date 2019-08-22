/*
Copyright 2019 The Stash Authors.

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

// Code generated by client-gen. DO NOT EDIT.

package v1beta1

import (
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	rest "k8s.io/client-go/rest"
	v1beta1 "stash.appscode.dev/stash/apis/stash/v1beta1"
	scheme "stash.appscode.dev/stash/client/clientset/versioned/scheme"
)

// BackupBlueprintsGetter has a method to return a BackupBlueprintInterface.
// A group's client should implement this interface.
type BackupBlueprintsGetter interface {
	BackupBlueprints() BackupBlueprintInterface
}

// BackupBlueprintInterface has methods to work with BackupBlueprint resources.
type BackupBlueprintInterface interface {
	Create(*v1beta1.BackupBlueprint) (*v1beta1.BackupBlueprint, error)
	Update(*v1beta1.BackupBlueprint) (*v1beta1.BackupBlueprint, error)
	Delete(name string, options *v1.DeleteOptions) error
	DeleteCollection(options *v1.DeleteOptions, listOptions v1.ListOptions) error
	Get(name string, options v1.GetOptions) (*v1beta1.BackupBlueprint, error)
	List(opts v1.ListOptions) (*v1beta1.BackupBlueprintList, error)
	Watch(opts v1.ListOptions) (watch.Interface, error)
	Patch(name string, pt types.PatchType, data []byte, subresources ...string) (result *v1beta1.BackupBlueprint, err error)
	BackupBlueprintExpansion
}

// backupBlueprints implements BackupBlueprintInterface
type backupBlueprints struct {
	client rest.Interface
}

// newBackupBlueprints returns a BackupBlueprints
func newBackupBlueprints(c *StashV1beta1Client) *backupBlueprints {
	return &backupBlueprints{
		client: c.RESTClient(),
	}
}

// Get takes name of the backupBlueprint, and returns the corresponding backupBlueprint object, and an error if there is any.
func (c *backupBlueprints) Get(name string, options v1.GetOptions) (result *v1beta1.BackupBlueprint, err error) {
	result = &v1beta1.BackupBlueprint{}
	err = c.client.Get().
		Resource("backupblueprints").
		Name(name).
		VersionedParams(&options, scheme.ParameterCodec).
		Do().
		Into(result)
	return
}

// List takes label and field selectors, and returns the list of BackupBlueprints that match those selectors.
func (c *backupBlueprints) List(opts v1.ListOptions) (result *v1beta1.BackupBlueprintList, err error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	result = &v1beta1.BackupBlueprintList{}
	err = c.client.Get().
		Resource("backupblueprints").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Do().
		Into(result)
	return
}

// Watch returns a watch.Interface that watches the requested backupBlueprints.
func (c *backupBlueprints) Watch(opts v1.ListOptions) (watch.Interface, error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	opts.Watch = true
	return c.client.Get().
		Resource("backupblueprints").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Watch()
}

// Create takes the representation of a backupBlueprint and creates it.  Returns the server's representation of the backupBlueprint, and an error, if there is any.
func (c *backupBlueprints) Create(backupBlueprint *v1beta1.BackupBlueprint) (result *v1beta1.BackupBlueprint, err error) {
	result = &v1beta1.BackupBlueprint{}
	err = c.client.Post().
		Resource("backupblueprints").
		Body(backupBlueprint).
		Do().
		Into(result)
	return
}

// Update takes the representation of a backupBlueprint and updates it. Returns the server's representation of the backupBlueprint, and an error, if there is any.
func (c *backupBlueprints) Update(backupBlueprint *v1beta1.BackupBlueprint) (result *v1beta1.BackupBlueprint, err error) {
	result = &v1beta1.BackupBlueprint{}
	err = c.client.Put().
		Resource("backupblueprints").
		Name(backupBlueprint.Name).
		Body(backupBlueprint).
		Do().
		Into(result)
	return
}

// Delete takes name of the backupBlueprint and deletes it. Returns an error if one occurs.
func (c *backupBlueprints) Delete(name string, options *v1.DeleteOptions) error {
	return c.client.Delete().
		Resource("backupblueprints").
		Name(name).
		Body(options).
		Do().
		Error()
}

// DeleteCollection deletes a collection of objects.
func (c *backupBlueprints) DeleteCollection(options *v1.DeleteOptions, listOptions v1.ListOptions) error {
	var timeout time.Duration
	if listOptions.TimeoutSeconds != nil {
		timeout = time.Duration(*listOptions.TimeoutSeconds) * time.Second
	}
	return c.client.Delete().
		Resource("backupblueprints").
		VersionedParams(&listOptions, scheme.ParameterCodec).
		Timeout(timeout).
		Body(options).
		Do().
		Error()
}

// Patch applies the patch and returns the patched backupBlueprint.
func (c *backupBlueprints) Patch(name string, pt types.PatchType, data []byte, subresources ...string) (result *v1beta1.BackupBlueprint, err error) {
	result = &v1beta1.BackupBlueprint{}
	err = c.client.Patch(pt).
		Resource("backupblueprints").
		SubResource(subresources...).
		Name(name).
		Body(data).
		Do().
		Into(result)
	return
}
