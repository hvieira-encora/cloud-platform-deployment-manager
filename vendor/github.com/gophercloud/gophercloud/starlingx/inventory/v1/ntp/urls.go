/* SPDX-License-Identifier: Apache-2.0 */
/* Copyright(c) 2019 Wind River Systems, Inc. */

package ntp

import "github.com/gophercloud/gophercloud"

func resourceURL(c *gophercloud.ServiceClient, id string) string {
	return c.ServiceURL("intp", id)
}

func rootURL(c *gophercloud.ServiceClient) string {
	return c.ServiceURL("intp")
}

func getURL(c *gophercloud.ServiceClient, id string) string {
	return resourceURL(c, id)
}

func listURL(c *gophercloud.ServiceClient) string {
	return rootURL(c)
}

func updateURL(c *gophercloud.ServiceClient, id string) string {
	return resourceURL(c, id)
}
