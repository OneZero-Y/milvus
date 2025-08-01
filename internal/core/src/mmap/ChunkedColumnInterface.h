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
#pragma once

#include "cachinglayer/CacheSlot.h"
#include "common/Chunk.h"

namespace milvus {

using namespace milvus::cachinglayer;

class ChunkedColumnInterface {
 public:
    virtual ~ChunkedColumnInterface() = default;

    // Default implementation does nothing.
    virtual void
    ManualEvictCache() const {
    }

    // Get raw data pointer of a specific chunk
    virtual cachinglayer::PinWrapper<const char*>
    DataOfChunk(int chunk_id) const = 0;

    // Check if the value at given offset is valid (not null)
    virtual bool
    IsValid(size_t offset) const = 0;

    // fn: (bool is_valid, size_t offset) -> void
    // If offsets is nullptr, this function will iterate over all rows.
    // Only BulkRawStringAt and BulkIsValid allow offsets to be nullptr.
    // Other Bulk* methods can also support nullptr offsets, but not added at this moment.
    virtual void
    BulkIsValid(std::function<void(bool, size_t)> fn,
                const int64_t* offsets,
                int64_t count) const = 0;

    // Check if the column can contain null values
    virtual bool
    IsNullable() const = 0;

    // Get total number of rows in the column
    virtual size_t
    NumRows() const = 0;

    // Get total number of chunks in the column
    virtual int64_t
    num_chunks() const = 0;

    // Get total byte size of the column data
    virtual size_t
    DataByteSize() const = 0;

    // Get number of rows in a specific chunk
    virtual int64_t
    chunk_row_nums(int64_t chunk_id) const = 0;

    virtual PinWrapper<SpanBase>
    Span(int64_t chunk_id) const = 0;

    virtual PinWrapper<
        std::pair<std::vector<std::string_view>, FixedVector<bool>>>
    StringViews(int64_t chunk_id,
                std::optional<std::pair<int64_t, int64_t>> offset_len =
                    std::nullopt) const = 0;

    virtual PinWrapper<std::pair<std::vector<ArrayView>, FixedVector<bool>>>
    ArrayViews(int64_t chunk_id,
               std::optional<std::pair<int64_t, int64_t>> offset_len) const = 0;

    virtual PinWrapper<std::vector<VectorArrayView>>
    VectorArrayViews(int64_t chunk_id) const = 0;

    virtual PinWrapper<
        std::pair<std::vector<std::string_view>, FixedVector<bool>>>
    ViewsByOffsets(int64_t chunk_id,
                   const FixedVector<int32_t>& offsets) const = 0;

    // Convert a global offset to (chunk_id, offset_in_chunk) pair
    virtual std::pair<size_t, size_t>
    GetChunkIDByOffset(int64_t offset) const = 0;

    virtual std::pair<std::vector<milvus::cachinglayer::cid_t>,
                      std::vector<int64_t>>
    GetChunkIDsByOffsets(const int64_t* offsets, int64_t count) const = 0;

    virtual PinWrapper<Chunk*>
    GetChunk(int64_t chunk_id) const = 0;

    virtual std::vector<PinWrapper<Chunk*>>
    GetAllChunks() const = 0;

    // Get number of rows before a specific chunk
    virtual int64_t
    GetNumRowsUntilChunk(int64_t chunk_id) const = 0;

    // Get vector of row counts before each chunk
    virtual const std::vector<int64_t>&
    GetNumRowsUntilChunk() const = 0;

    virtual void
    BulkValueAt(std::function<void(const char*, size_t)> fn,
                const int64_t* offsets,
                int64_t count) = 0;

    virtual void
    BulkPrimitiveValueAt(void* dst, const int64_t* offsets, int64_t count) = 0;

    virtual void
    BulkVectorValueAt(void* dst,
                      const int64_t* offsets,
                      int64_t element_sizeof,
                      int64_t count) = 0;

    // fn: (std::string_view value, size_t offset, bool is_valid) -> void
    // If offsets is nullptr, this function will iterate over all rows.
    // Only BulkRawStringAt and BulkIsValid allow offsets to be nullptr.
    // Other Bulk* methods can also support nullptr offsets, but not added at this moment.
    virtual void
    BulkRawStringAt(std::function<void(std::string_view, size_t, bool)> fn,
                    const int64_t* offsets = nullptr,
                    int64_t count = 0) const {
        ThrowInfo(ErrorCode::Unsupported,
                  "BulkRawStringAt only supported for ChunkColumnInterface of "
                  "variable length type");
    }

    virtual void
    BulkRawJsonAt(std::function<void(Json, size_t, bool)> fn,
                  const int64_t* offsets,
                  int64_t count) const {
        ThrowInfo(
            ErrorCode::Unsupported,
            "RawJsonAt only supported for ChunkColumnInterface of Json type");
    }

    virtual void
    BulkArrayAt(std::function<void(ScalarFieldProto&&, size_t)> fn,
                const int64_t* offsets,
                int64_t count) const {
        ThrowInfo(ErrorCode::Unsupported,
                  "BulkArrayAt only supported for ChunkedArrayColumn");
    }

    virtual void
    BulkVectorArrayAt(std::function<void(VectorFieldProto&&, size_t)> fn,
                      const int64_t* offsets,
                      int64_t count) const {
        ThrowInfo(
            ErrorCode::Unsupported,
            "BulkVectorArrayAt only supported for ChunkedVectorArrayColumn");
    }

    static bool
    IsPrimitiveDataType(DataType data_type) {
        return data_type == DataType::INT8 || data_type == DataType::INT16 ||
               data_type == DataType::INT32 || data_type == DataType::INT64 ||
               data_type == DataType::FLOAT || data_type == DataType::DOUBLE ||
               data_type == DataType::BOOL;
    }

    static bool
    IsChunkedVariableColumnDataType(DataType data_type) {
        return data_type == DataType::STRING ||
               data_type == DataType::VARCHAR || data_type == DataType::TEXT ||
               data_type == DataType::JSON;
    }

    static bool
    IsChunkedArrayColumnDataType(DataType data_type) {
        return data_type == DataType::ARRAY;
    }

    static bool
    IsChunkedVectorArrayColumnDataType(DataType data_type) {
        return data_type == DataType::VECTOR_ARRAY;
    }

    static bool
    IsChunkedColumnDataType(DataType data_type) {
        return !IsChunkedVariableColumnDataType(data_type) &&
               !IsChunkedArrayColumnDataType(data_type);
    }

 protected:
    std::pair<std::vector<milvus::cachinglayer::cid_t>, std::vector<int64_t>>
    ToChunkIdAndOffset(const int64_t* offsets, int64_t count) const {
        AssertInfo(offsets != nullptr, "Offsets cannot be nullptr");
        auto num_rows = NumRows();
        for (int64_t i = 0; i < count; i++) {
            if (offsets[i] < 0 || offsets[i] >= num_rows) {
                ThrowInfo(ErrorCode::OutOfRange,
                          "offsets[{}] {} is out of range, num_rows: {}",
                          i,
                          offsets[i],
                          num_rows);
            }
        }
        return GetChunkIDsByOffsets(offsets, count);
    }
};

}  // namespace milvus
