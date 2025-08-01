// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License

#pragma once

#include <string>
#include <boost/filesystem.hpp>

#include "index/InvertedIndexTantivy.h"
#include "index/IndexStats.h"

namespace milvus::index {

using stdclock = std::chrono::high_resolution_clock;
class TextMatchIndex : public InvertedIndexTantivy<std::string> {
 public:
    // for growing segment.
    explicit TextMatchIndex(int64_t commit_interval_in_ms,
                            const char* unique_id,
                            const char* tokenizer_name,
                            const char* analyzer_params);
    // for sealed segment to create index from raw data during loading.
    explicit TextMatchIndex(const std::string& path,
                            const char* unique_id,
                            uint32_t tantivy_index_version,
                            const char* tokenizer_name,
                            const char* analyzer_params);
    // for building index.
    explicit TextMatchIndex(const storage::FileManagerContext& ctx,
                            uint32_t tantivy_index_version,
                            const char* tokenizer_name,
                            const char* analyzer_params);
    // for loading built index
    explicit TextMatchIndex(const storage::FileManagerContext& ctx);

 public:
    IndexStatsPtr
    Upload(const Config& config) override;

    void
    Load(const Config& config);

 public:
    void
    AddTextSealed(const std::string& text, const bool valid, int64_t offset);

    void
    AddNullSealed(int64_t offset);

    void
    AddTextsGrowing(size_t n,
                    const std::string* texts,
                    const bool* valids,
                    int64_t offset_begin);

    void
    BuildIndexFromFieldData(const std::vector<FieldDataPtr>& field_datas,
                            bool nullable);

    void
    Finish();

    void
    Commit();

    void
    Reload();

 public:
    void
    CreateReader(SetBitsetFn set_bitset);

    void
    RegisterTokenizer(const char* tokenizer_name, const char* analyzer_params);

    TargetBitmap
    MatchQuery(const std::string& query);

    TargetBitmap
    PhraseMatchQuery(const std::string& query, uint32_t slop);

 private:
    bool
    shouldTriggerCommit();

 private:
    mutable std::mutex mtx_;
    std::atomic<stdclock::time_point> last_commit_time_;
    int64_t commit_interval_in_ms_;
};
}  // namespace milvus::index
