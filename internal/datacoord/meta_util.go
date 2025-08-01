// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package datacoord

import (
	"github.com/cockroachdb/errors"

	"github.com/milvus-io/milvus/pkg/v2/proto/datapb"
)

var ErrIgnoredSegmentMetaOperation = errors.New("ignored segment meta operation")

// reviseVChannelInfo will revise the datapb.VchannelInfo for upgrade compatibility from 2.0.2
func reviseVChannelInfo(vChannel *datapb.VchannelInfo) {
	removeDuplicateSegmentIDFn := func(ids []int64) []int64 {
		result := make([]int64, 0, len(ids))
		existDict := make(map[int64]bool)
		for _, id := range ids {
			if _, ok := existDict[id]; !ok {
				existDict[id] = true
				result = append(result, id)
			}
		}
		return result
	}

	if vChannel == nil {
		return
	}
	// if the segment infos is not nil(generated by 2.0.2), append the corresponding IDs to segmentIDs
	// and remove the segment infos, remove deplicate ids in case there are some mixed situations
	if vChannel.FlushedSegments != nil && len(vChannel.FlushedSegments) > 0 {
		for _, segment := range vChannel.FlushedSegments {
			vChannel.FlushedSegmentIds = append(vChannel.GetFlushedSegmentIds(), segment.GetID())
		}
		vChannel.FlushedSegments = []*datapb.SegmentInfo{}
	}
	vChannel.FlushedSegmentIds = removeDuplicateSegmentIDFn(vChannel.GetFlushedSegmentIds())

	if vChannel.UnflushedSegments != nil && len(vChannel.UnflushedSegments) > 0 {
		for _, segment := range vChannel.UnflushedSegments {
			vChannel.UnflushedSegmentIds = append(vChannel.GetUnflushedSegmentIds(), segment.GetID())
		}
		vChannel.UnflushedSegments = []*datapb.SegmentInfo{}
	}
	vChannel.UnflushedSegmentIds = removeDuplicateSegmentIDFn(vChannel.GetUnflushedSegmentIds())

	if vChannel.DroppedSegments != nil && len(vChannel.DroppedSegments) > 0 {
		for _, segment := range vChannel.DroppedSegments {
			vChannel.DroppedSegmentIds = append(vChannel.GetDroppedSegmentIds(), segment.GetID())
		}
		vChannel.DroppedSegments = []*datapb.SegmentInfo{}
	}
	vChannel.DroppedSegmentIds = removeDuplicateSegmentIDFn(vChannel.GetDroppedSegmentIds())
}
