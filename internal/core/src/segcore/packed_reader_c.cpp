// Copyright 2023 Zilliz
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#include "segcore/packed_reader_c.h"
#include "milvus-storage/packed/reader.h"
#include "milvus-storage/common/log.h"
#include "milvus-storage/filesystem/fs.h"
#include "milvus-storage/common/config.h"

#include <arrow/c/bridge.h>
#include <arrow/filesystem/filesystem.h>
#include <arrow/status.h>
#include <memory>
#include "common/EasyAssert.h"
#include "common/type_c.h"
#include "monitor/scope_metric.h"

CStatus
NewPackedReaderWithStorageConfig(char** paths,
                                 int64_t num_paths,
                                 struct ArrowSchema* schema,
                                 const int64_t buffer_size,
                                 CStorageConfig c_storage_config,
                                 CPackedReader* c_packed_reader) {
    SCOPE_CGO_CALL_METRIC();

    try {
        auto truePaths = std::vector<std::string>(paths, paths + num_paths);

        milvus_storage::ArrowFileSystemConfig conf;
        conf.address = std::string(c_storage_config.address);
        conf.bucket_name = std::string(c_storage_config.bucket_name);
        conf.access_key_id = std::string(c_storage_config.access_key_id);
        conf.access_key_value = std::string(c_storage_config.access_key_value);
        conf.root_path = std::string(c_storage_config.root_path);
        conf.storage_type = std::string(c_storage_config.storage_type);
        conf.cloud_provider = std::string(c_storage_config.cloud_provider);
        conf.iam_endpoint = std::string(c_storage_config.iam_endpoint);
        conf.log_level = std::string(c_storage_config.log_level);
        conf.region = std::string(c_storage_config.region);
        conf.useSSL = c_storage_config.useSSL;
        conf.sslCACert = std::string(c_storage_config.sslCACert);
        conf.useIAM = c_storage_config.useIAM;
        conf.useVirtualHost = c_storage_config.useVirtualHost;
        conf.requestTimeoutMs = c_storage_config.requestTimeoutMs;
        conf.gcp_credential_json =
            std::string(c_storage_config.gcp_credential_json);
        conf.use_custom_part_upload = c_storage_config.use_custom_part_upload;
        milvus_storage::ArrowFileSystemSingleton::GetInstance().Init(conf);
        auto trueFs = milvus_storage::ArrowFileSystemSingleton::GetInstance()
                          .GetArrowFileSystem();
        if (!trueFs) {
            return milvus::FailureCStatus(
                milvus::ErrorCode::FileReadFailed,
                "[StorageV2] Failed to get filesystem");
        }
        auto trueSchema = arrow::ImportSchema(schema).ValueOrDie();
        auto reader = std::make_unique<milvus_storage::PackedRecordBatchReader>(
            trueFs, truePaths, trueSchema, buffer_size);
        *c_packed_reader = reader.release();
        return milvus::SuccessCStatus();
    } catch (std::exception& e) {
        return milvus::FailureCStatus(&e);
    }
}

CStatus
NewPackedReader(char** paths,
                int64_t num_paths,
                struct ArrowSchema* schema,
                const int64_t buffer_size,
                CPackedReader* c_packed_reader) {
    SCOPE_CGO_CALL_METRIC();

    try {
        auto truePaths = std::vector<std::string>(paths, paths + num_paths);
        auto trueFs = milvus_storage::ArrowFileSystemSingleton::GetInstance()
                          .GetArrowFileSystem();
        if (!trueFs) {
            return milvus::FailureCStatus(
                milvus::ErrorCode::FileReadFailed,
                "[StorageV2] Failed to get filesystem");
        }
        auto trueSchema = arrow::ImportSchema(schema).ValueOrDie();
        auto reader = std::make_unique<milvus_storage::PackedRecordBatchReader>(
            trueFs, truePaths, trueSchema, buffer_size);
        *c_packed_reader = reader.release();
        return milvus::SuccessCStatus();
    } catch (std::exception& e) {
        return milvus::FailureCStatus(&e);
    }
}

CStatus
ReadNext(CPackedReader c_packed_reader,
         CArrowArray* out_array,
         CArrowSchema* out_schema) {
    SCOPE_CGO_CALL_METRIC();

    try {
        auto packed_reader =
            static_cast<milvus_storage::PackedRecordBatchReader*>(
                c_packed_reader);
        std::shared_ptr<arrow::RecordBatch> record_batch;
        auto status = packed_reader->ReadNext(&record_batch);
        if (!status.ok()) {
            return milvus::FailureCStatus(milvus::ErrorCode::FileReadFailed,
                                          status.ToString());
        }
        if (record_batch == nullptr) {
            // end of file
            return milvus::SuccessCStatus();
        } else {
            std::unique_ptr<ArrowArray> arr = std::make_unique<ArrowArray>();
            std::unique_ptr<ArrowSchema> schema =
                std::make_unique<ArrowSchema>();
            auto status = arrow::ExportRecordBatch(
                *record_batch, arr.get(), schema.get());
            if (!status.ok()) {
                return milvus::FailureCStatus(milvus::ErrorCode::FileReadFailed,
                                              status.ToString());
            }
            *out_array = arr.release();
            *out_schema = schema.release();
            return milvus::SuccessCStatus();
        }
        return milvus::SuccessCStatus();
    } catch (std::exception& e) {
        return milvus::FailureCStatus(&e);
    }
}

CStatus
CloseReader(CPackedReader c_packed_reader) {
    SCOPE_CGO_CALL_METRIC();

    try {
        auto packed_reader =
            static_cast<milvus_storage::PackedRecordBatchReader*>(
                c_packed_reader);
        packed_reader->Close();
        delete packed_reader;
        return milvus::SuccessCStatus();
    } catch (std::exception& e) {
        return milvus::FailureCStatus(&e);
    }
}