# 18. 목적지 메타데이터의 기록과 대조

이 설계는 ARCHIVE_PRIMARY 전체에서 무엇이 보관됐는지, 어떤 기록이 뒤처졌는지, 무엇을 근거로 복구할지를 정한다. 현재 기본 게시 형식은 v3다. 아래 7개 계약은 요구사항 카탈로그와 기본 통합 테스트에 연결되어 있다. [기존 패키지와 카탈로그](04-data-archive.md#6-아카이브-패키지와-보관됨) · [행사별 배치](17-source-classification-policy.md#3-실행과-목적지)

## 1. 원인과 재현 근거

2026-11-24 Windows 목적지·Control 작업 원장·설치된 검증기를 읽기 전용으로 대조했다. 조사 원장은 작업 공간과 Windows 고정 시험 목적지의 `evidence/destination-metadata-audit-20261124T091705/`에 보관했다. 아래 계약 테스트는 이 자료에서 관찰한 조건을 임시 저장소로 재현한다.

| 관측 | 코드와 기록에서 확인한 원인 |
|---|---|
| 운영 루트 16행 중 15행 일치. `VP-20261110-d2cf9253c403`의 공개 리비전은 4, CSV는 1 | 당시 STT 명세는 기준 1과 v3의 `.archive-package.json`을 지시했고 실제 패키지는 v2의 `metadata.json`을 사용했다. 작업 실패 세 건 직전에 리비전 2·3·4가 각각 게시됐다. 후속 카탈로그 Step은 대기에 남았다. CSV의 리비전·manifest·완료 시각·전사 JSON·텍스트·자막 경로가 뒤처졌다. |
| 렌더 실패 실행에 CSV 5행과 기록 시각이 표시됨 | 작업 공간이 입력 파일 수와 분류 완료 시각을 카탈로그 값으로 초기화한다. 실제 해당 실행의 패키지 출력과 루트 행은 0개다. 기존 시각 테스트는 1단계 카탈로그가 성공한 뒤부터 비교해 이 사례를 놓쳤다. |
| 행사 시험 목적지의 영상 4개가 검사에서 누락됨 | 복구 파일명이 v2·v3와 다른 폴더를 검증기가 건너뛰고 빈 검사 목록에 `Current() == true`를 반환한다. 행사 2곳에 MP4·개별 JSON·SRT가 각 4개 있고 영상 해시가 모두 맞았으나 설치된 CLI는 `current=true/entries=0`을 기록했다. |
| 지연된 게시와 불완전한 재생성 | 파일 잠금은 쓰기를 직렬화하지만 현재 병합은 같은 ID의 나중 요청으로 행을 대체한다. 임시 StorageRoot에서 리비전 4 행을 1로 덮어쓰는 사례와 미등록 영상이 있는데도 재생성이 성공하는 사례를 재현했다. 이 둘은 별도 재현 결과다. |

조사 당시 코드는 **새** STT 명세에 1단계 게시 경로를 전달하고, 같은 기준 리비전의 바로 다음 게시를 재사용한다. 과거에 저장된 잘못된 명세는 파일 수정 **후** 경로 대조에 걸린다. 당시 Worker에서도 v2 패키지가 리비전 1에서 2로 바뀐 직후 실패하는 것을 재현했다. 수정된 Worker는 패키지 잠금 아래에서 이 경로를 쓰기 전에 대조한다. [명세의 경로 전달](../../internal/application/archivetranscription/projection.go) · [Worker의 수정과 결과 확인](../../internal/application/workerexecutor/media.go)

운영 v2·v3 패키지 16개의 원본 16행에서 `video-info.csv` 23열과 패키지 원본 정보는 일치했다. v1 패키지 3개는 별도 구형 원장의 원본 46행에 대응했다. v1의 원본 행 수를 최신 패키지 행 수에 더하지 않는다.

## 2. 기록의 소유권

| 업무 질문 | 기준 기록과 집계 단위 |
|---|---|
| 어느 영상과 자막이 실제로 보관됐는가? | 목적지의 버전별 패키지 복구 문서와 검증된 공개 파일. 식별자는 package ID·revision·manifest digest다. 원본별 상대경로·checksum·기술정보는 패키지에서 읽는다. |
| 드라이브 전체에서 어떤 결과를 찾을 수 있는가? | 루트 `archive-catalog.csv`는 검증된 결과 패키지 하나당 한 행인 검색용 색인이다. 같은 행사명·채널명이 반복되어도 ID로 구분한다. |
| 이 실행이 몇 건을 기록했는가? | 성공한 `archive_catalog` Step의 영속 출력에 `catalog_publication`을 둔다. 목적지 Storage ID·ArchiveRun ID·적용한 `packages[]`의 package ID·revision·manifest digest·게시 CSV SHA-256을 고정한다. |
| 최근 검사에서 어디까지 확인했는가? | Worker 보고서에 목적지 ID·볼륨 식별 근거·검사 시각·CSV 해시 또는 부재·범위·건별 차이를 남긴다. Control에는 보고서 ID·checksum과 제한된 요약을 보관한다. |

화면은 해당 실행·목적지의 성공 기록에서 고유 package ID의 합집합을 구한다. STT 갱신과 재전송은 기존 패키지 한 건으로 센다. 게시 이전에 실패한 실행은 0건·기록 시각 없음이다. 성공 시각은 가장 최근 성공한 카탈로그 Step 완료 시각이다. 과거 성공 기록에 ID 목록이 없으면 같은 실행의 출력·명세·게시 검증 기록으로 귀속할 수 있는 건만 복원하고 확인할 수 없는 수치는 ‘기록 없음’으로 표시한다. `DSH-027`, `DSH-046`

## 3. 목적지 전체를 검증하는 방법

1. 루트의 단독 영상과 한 단계 결과 폴더 안의 영상·복구 문서·행사 manifest를 **독립적으로** 찾는다. 규정 깊이를 넘는 결과 구조나 접근하지 못한 경로는 범위 근거에 남긴다. `archive.StorageRoot` 경계를 사용하고 외부 링크·junction은 따라가지 않는다. 내부 잠금과 staging은 제외 사유와 함께 집계한다. 구형 `packages/`, `catalog/`는 v1 전용 reader가 현재 포인터와 구형 원장을 대조해 패키지 수·원본 행 수를 별도 보고한다. 읽지 못한 v1 영역은 미완료로 남긴다.
2. 결과 영상 후보마다 검증된 패키지 귀속 또는 미해결 상대경로 하나를 기록한다. 문서만 있고 영상이 사라진 경우는 패키지 오류다. 빈 폴더나 일반 문서는 영상 후보가 아니다. 참조와 디렉터리 항목을 같은 상대경로로 묶어 중복 계수하지 않는다.
3. `coverage`에는 `state`, `candidate_videos`, `unresolved_videos`, layout별 계수와 접근 누락 사유를 둔다. 전 범위를 읽고 후보·패키지·원장 행이 모두 0이면 `EMPTY`, 범위를 설명할 수 있으면 `COMPLETE`, 미등록 영상·미지원 구조·접근 누락이 있으면 `INCOMPLETE`다. 행별 기존 상태에는 `CURRENT`, `CATALOG_MISSING`, `CATALOG_ORPHAN`, `CATALOG_MISMATCH`, `LAYOUT_INVALID`를 사용한다. 전체 `current=true`는 COMPLETE이고 대상이 있으며 모든 대조 결과가 일치할 때만 허용한다. CLI 호출 성공 여부는 별도 기록이다. `ARC-017`, `ARC-022`
4. 전체 검증은 목적지 파일을 스트리밍으로 해시한다. 원본 드라이브를 연결할 필요가 없다. 동일 회차의 중복 파일 읽기를 피하고 heartbeat·목록 조회는 완료 보고서를 읽는다. 검사 중 CSV나 패키지 리비전이 바뀌면 해당 관측을 다시 확인하거나 미완료로 보고한다.
5. v3에서는 현재 복구 문서의 원본 목록을 게시 때 사용한 23열로 투영해 실제 `video-info.csv`의 원본 순서와 각 필드를 대조한다. 원본 행과 렌더 결과는 해상도가 다를 수 있다. 필드가 충돌하면 패키지 ID·원본 행 순번·열 이름과 `LAYOUT_INVALID`를 기록한다. 재생성도 같은 검사를 거쳐 `ARCHIVE_SOURCE_METADATA_MISMATCH`를 반환하고 기존 CSV를 보존한다. `ARC-017`, `ARC-018`

## 4. 행사별 메타데이터

v2는 `current.json`·`metadata.json`, v3는 `.archive-package.json`을 사용한다. 정식 v4에는 행사 폴더와 영상 패키지 두 수준의 식별자가 있다. 한 목적지의 기존 v2·v3 행과 공존한다. [행사 명명과 패키지 배치](17-source-classification-policy.md#3-실행과-목적지)

| v4 기록 | 검증할 내용 |
|---|---|
| `<행사>/event.json` | `layout_version=4`, 안정된 `occasion_id`, 행사 `revision`, `channels[]`의 고유 `package_id, metadata_file, metadata_sha256`. 각 참조는 해당 행사 직속 파일명이다. 중복 소유, Windows 대소문자 충돌, 절대경로·상위 이동을 거부한다. |
| `<채널>.json` | `schema_version=1`, layout·패키지 ID·revision·상태, 실행·입력 revision·원본 드라이브·판정·Profile, 결과 경로·처리일, 순서 있는 `sources[]`, QC 실측 `output_inspection`, `files[]`의 역할·직속 경로·크기·SHA-256. 이 JSON의 저장된 바이트 SHA-256이 채널 manifest digest다. 문서 안에는 자신의 해시를 넣지 않는다. |
| 영상·자막·근거 | 원본의 기술정보와 결과 영상의 길이·크기·해상도·codec·비트레이트를 각각 보존한다. 소스 합본의 `source_timeline`에는 입력 순서와 마커 시각을 둔다. STT 게시의 `files[]`는 `SUBTITLE`, `TRANSCRIPT`, `TRANSCRIPT_TEXT` 역할과 해시를 기록하고 `transcript_quality`는 품질 사유·건수를 보존한다. 미관측값은 추정하지 않는다. |
| `archive-catalog.csv` | 기존 검색 열을 유지한다. 같은 `result_folder`에 여러 행이 있을 수 있다. package ID와 결과 영상 상대경로는 각각 유일하다. 각 행에서 채널 JSON과 게시된 자막으로 이동한다. `ARC-025` |

후속 v4 쓰기 설계에서 행사 내 추가 게시는 행사 잠금 아래 이전 manifest revision을 확인하고 검증된 채널을 합친 뒤 행사 manifest를 마지막에 교체한다. 같은 이름의 공개 파일을 갱신할 때는 이전 revision bytes와 교체 의도를 먼저 내구성 있게 기록한다. 중단 시 이전 또는 새 manifest와 해시가 맞는 파일 묶음으로 복구한 뒤 읽기를 재개한다. 미완료 묶음은 정상 행으로 게시하지 않는다. 현재 시험 JSON은 원본 경로와 영상 checksum을 제공하지만 정식 package ID·revision 계약이 없다. 별도 원본 대조와 패키지 식별을 거치기 전에는 미해결 시험 결과로 보고한다.

## 5. 게시 중단과 복구

v3의 STT 리비전은 직전 manifest에서 이어받을 공개 영상과 `video-info.csv`의 크기·SHA-256을 패키지 잠금 아래 대조한다. 확인한 항목을 새 manifest에 그대로 싣고 전사 산출물을 추가한다. 파일이 다르면 첫 sidecar 쓰기 전에 `ARCHIVE_PACKAGE_FILE_MISMATCH`로 끝난다. 완료 뒤 같은 전사를 재전송하거나 카탈로그만 복구할 때도 현재 공개 파일을 검증한 뒤에만 성공 출력을 반환한다. `WRK-018`

v2는 다음 리비전의 불변 metadata를 먼저 기록해 최초 게시 시각을 고정한다. 포인터 교체가 중단되면 같은 요청으로 원래 시각의 후보를 다시 만들고, 남은 불변 파일 전체의 bytes가 일치할 때 공개 파일과 포인터의 게시를 마친다. 포인터가 이전 리비전을 가리키는 동안 다음 리비전의 공개 sidecar를 이전 게시의 성공 근거로 사용할 수 없다. 다른 전사로 재전송하면 충돌을 반환한다. `ARC-011`, `WRK-018`

같은 목적지의 두 패키지는 각각의 패키지 잠금으로 갱신할 수 있다. 루트 CSV를 출력하는 두 Registry 실행은 같은 출력 잠금을 사용한다. A의 게시 뒤 B가 자기 행을 갱신하면 A의 정상 재전송은 A의 기존 revision·manifest와 B의 변경을 포함한 CSV 해시를 반환한다. A 자체가 명세의 기준을 두 리비전 이상 앞서면 재전송을 거부한다. 패키지 잠금 해제 뒤에는 공개 metadata에서 명세와 일치하는 리비전을 읽은 동일한 bytes로 checksum을 계산한다. 그사이 게시된 다른 전사의 리비전을 이 작업의 성공 근거로 반환할 수 없다. `WRK-018`

- **수정 전 검사.** 패키지 잠금 아래 실제 ID·layout·현재 revision·metadata 상대경로를 읽고 고정 명세와 비교한다. 경로가 다르면 첫 파일 쓰기 전에 `PACKAGE_LAYOUT_PATH_MISMATCH`를 기록한다. 올바른 명세가 재전송되면 같은 기준의 바로 다음 리비전과 세 sidecar checksum이 정확히 맞을 때만 기존 결과를 돌려준다. 더 뒤의 revision은 오래된 작업과 별도로 대조한다. `ARC-011`, `ARC-021`
- **루트 병합.** 패키지 게시와 CSV 교체 사이에 중단되면 `CATALOG_PENDING` 근거를 남긴다. 루트 잠금 아래 더 낮은 리비전, 같은 리비전의 다른 digest 또는 충돌하는 행은 거부하고 동일한 행은 같은 checksum으로 멱등 처리한다. 더 높은 리비전은 현재 패키지와 대조한 뒤 반영한다. 다른 패키지의 행은 유지한다. `ARC-024`
- **재생성.** 먼저 읽기 전용 보고서에 누락·변경·고아 행과 이전/이후 revision·digest를 기록한다. 미등록 결과나 읽기 누락이 있으면 전체 CSV 교체를 중단한다. 적용할 때 목적지 identity·기준 CSV 해시·관련 manifest를 다시 대조한다. 여러 잠금이 동시에 필요하면 목적지 카탈로그 → 행사 → 패키지 순으로 잡는다. 일반 패키지 게시는 패키지 잠금을 놓은 뒤 루트 게시를 요청한다. 범위를 확인한 경우에만 임시 CSV를 재파싱·동기화·원자 교체하고 재검증 보고서를 연결한다. `ARC-018`, `ARC-023`
- **실측 1건의 처리.** v2 리비전 4의 영상·자막·전사·텍스트 해시와 루트 행 리비전 1을 최신 보고서에서 확인한다. 다른 패키지를 포함한 전체 범위가 설명되면 리비전 4의 행 투영만 복구한다. 적용 전후 CSV 해시, 대상 ID와 여섯 열의 차이, 나머지 행의 보존, 사후 불일치 0건을 기록한다. 렌더·재전사·리비전 5 생성은 필요하지 않다. 원래 FAILED Job은 이력으로 두고 별도 복구 실행을 연결한다. 파일 자체가 다르면 패키지 검토로 분기한다.

## 6. 아키비스트가 확인할 근거

관리 검증은 `archive-catalog-maintenance` Job의 `verify` Step에서 실행한다. 결과 JSON은 별도 보고서 Storage에 게시하고, Control은 성공한 Worker 출력에서 요약과 보고서 checksum을 읽는다. 직접 CLI로 검사하면 같은 형식의 로컬 보고서를 얻는다. 읽기 전용 `archive.system.status.get.v1`에 `destination_catalogs[]`를 추가한다. 목적지 Storage ID별로 마지막 검증 보고서 ID·관측 시각·CSV SHA-256·일치·불일치·미해결 수·다음 확인 항목을 제공한다. 시스템 상태는 원본 선택이나 실행 고정 없이 조회한다. 상세 목록은 보고서 참조로 읽고 기본 응답에는 파일 경로나 자막 원문을 싣지 않는다. 알려진 게시 뒤 사후 대조 전이면 `STALE`, 미관측은 `NOT_OBSERVED`, 빈 목적지는 `EMPTY`, 부분 조사는 `INCOMPLETE`, 확인된 행 차이는 `MISMATCH`로 표시한다. 범위가 불완전하면서 차이도 발견된 경우에는 두 근거를 모두 보존한다. 이전 보고서만으로 오프라인 목적지가 현재 일치한다고 표시하지 않는다. 읽기 요청 자체는 복구 명령을 발생시키지 않는다. `MCP-014`

## 7. 구현 순서와 인수

| 순서 | 요구사항 | 대응 테스트 |
|---|---|---|
| 1 | ARC-021 | TestDestinationRevisionRefusesWrongLayoutBeforePublishing |
| 1 | ARC-022 | TestDestinationVerificationAccountsForUnindexedVideos |
| 1 | ARC-023 | TestDestinationRebuildRetainsCatalogWhenCoverageIsIncomplete |
| 1 | ARC-024 | TestDestinationCatalogRefusesOlderOrConflictingPublication |
| 2 | DSH-046 | TestDestinationRunSummaryCountsOnlyProvenPackagePublications |
| 2 | MCP-014 | TestDestinationStatusReturnsScopedVerificationEvidenceWithoutRun |
| 3 | ARC-025 | TestDestinationEventCatalogUsesOneRowPerVerifiedChannel |

개별 계약은 `go test -run '^TestDestination' -count=1 ./internal/adapter/archivecatalog ./internal/application/workerexecutor ./internal/application/archiveintakeworkspace ./internal/adapter/httpmcp`로 실행한다. 전체 요구사항과 테스트는 `make verify`로 대조한다.

Windows 인수는 고정 시험 목적지에서 수행한다. 구형 원장과 v2·v3·v4, 미등록 행사, 리비전 1 CSV와 리비전 4 패키지를 재현한다. CSV 교체 직전·직후 중단, 같은 요청 재전송, 구버전 게시를 시험한다. 재생성 전후 파일 해시와 행 차이, Archivist가 읽은 목적지별 보고서와 독립적인 파일 대조 결과가 일치해야 운영 복구로 넘어간다.

### Windows 검증 결과 (2026-11-24)

| 읽은 목적지 | 범위와 대조 결과 |
|---|---|
| 운영 `C:/ArchiveStorage` | 영상 후보 19개. v2 15개·v3 1개 중 루트 CSV와 일치 15개, 불일치 1개. v1 패키지 3개와 구형 원본 원장 46행은 별도로 대조했다. 미해결 영상 0개. |
| 고정 시험 목적지의 `output` | 행사 2개, 영상 후보 4개, 미해결 영상 4개. 두 행사 문서에 정식 채널 참조가 없어 검사 상태는 `INCOMPLETE`였다. |
| 격리된 재생성 시험 | 미등록 영상이 있으면 CSV 교체를 중단했다. 이를 해소한 경우 리비전 1 행을 4로 복원하고 다른 행과 패키지 파일의 해시를 보존했다. |

운영 차이 한 건은 `VP-20261110-d2cf9253c403`의 리비전·manifest·완료 시각·전사 JSON·텍스트·자막 경로다. 읽기 전용 검사의 루트 CSV SHA-256은 이전 조사와 같은 `f7750e591028e040e23da14c751ea4eea6680ff00f430dbc902cf7acc887a704`이었다. Windows 시험 로그와 원본 보고서는 작업 공간의 `evidence/destination-catalog-acceptance-20261124T101124/` 아래에 보관했다. 운영 복구는 별도 작업으로 남아 있다.
