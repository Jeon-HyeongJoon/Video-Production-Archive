# 13. Archive Intake 설계

## 1. 목적과 결정 경계

담당자는 Worker PC에 연결된 원본 드라이브에서 폴더 범위를 고르고, 같은 Worker가 접근하는 아카이브 저장소에 최적화 기록물을 보관한다. 이 업무는 스냅샷 복제가 아니다. 원본 추적 정보와 검증된 결과를 사람이 관리·검색·복원할 수 있는 ArchivePackage와 ArchiveCatalog로 남기는 일이다.

2026-10-31에 다음을 결정했다.

- 아카이브 저장소는 `StorageRole.ARCHIVE_PRIMARY`다. 현장 표현인 “백업 하드”도 이 뜻이다. 기술적 복제본은 `BACKUP_OFFSITE`에만 둔다.
- 화면 이름은 “아카이브”이며 검색 화면과 같은 기록물을 가리킨다.
- 로컬 Storage, 연결된 네트워크 공유, 원본과 다른 새 외장 드라이브 및 검증된 FTP·SFTP 원격 파일 서비스 Storage를 허용한다. SMTP는 아카이브 저장소가 아니라 전달 채널이다.
- 담당자만 Worker·원본 드라이브·아카이브 저장소·폴더 범위를 고르고 영상 정리·보관을 한 번 시작한다. Archivist와 자동 정책은 Storage나 처리 범위를 고르지 않는다. 1단계 게이트가 열리면 Control이 별도 STT Job을 자동으로 이어서 시작한다. 담당자가 고정한 범위 안의 HOLD는 Archivist가 구조화된 근거로 명시적으로 분류할 수 있다.
- ARCHIVE_PRIMARY에 한 벌을 검증해 게시하면 “보관됨”이다. `ARC-004`의 복제본 검증과 사람 승인을 충족하면 “확정됨”이다.
- 기록물은 중앙 DB 없이 복원 가능한 ArchivePackage 형식을 따른다. 패키지 게시 형식은 현재 수직 경로에 연결되어 있지만, 패키지만으로 DB를 복원하는 명령과 acceptance test인 `ARC-007`은 planned다.
- 원본 드라이브 분리와 네트워크 아카이브 저장소 끊김은 `WaitReason.DEVICE`다. 기다리는 동안 실행 시도 횟수를 소비하지 않는다.
- 폴더별 단계 표시는 드라이브당 단계 상태 행렬과 진행 피드백으로 표시한다. 분기·선택 gate·독립 기록을 하나의 순차선으로 잇지 않으며 단계별 세부는 노드와 폴더 행 세부 설명에서 확인한다.

아카이브 실행 개체는 기존 run 영수증과 분리된 영속 `ArchiveRun`다. 2026-10-31에 Q1~Q12의 권장안을 모두 v1 계약으로 확정했다. `ORG-012`

## 2. 선택과 탐색

담당자는 최상위 `작업 생성 > 영상 정리·보관`에서 원본 드라이브, 아카이브 저장소, 폴더 범위를 차례로 고를 수 있다. 선택 참조가 없는 ArchivistTurn에서는 인증된 Archivist가 MCP로 현재 후보를 조회하고 후보 ID·revision을 명시적으로 제출해 같은 선택을 고정할 수 있다. Worker는 선택한 원본 드라이브의 현재 연결 Worker로 정해지며 별도로 선택하지 않는다. 작업 관리와 장비·에이전트의 드라이브 상세는 이 선택을 만들지 않는다. `DEV-010`, `DSH-010`, `MCP-003`, `COL-003`

Windows에서 외부 data bus Volume이 연결되면 Worker는 파일을 읽지 않고 PhysicalDevice·volume identity, 역할이 다른 source·archive Storage identity, label·filesystem·capacity만 후보로 보고한다. USB bridge가 volume을 `DRIVE_FIXED`로 노출해도 외부 bus면 후보이며 내부 시스템 volume과 작은 보조 partition은 제외한다. drive letter와 mount root는 Worker 밖으로 보내지 않는다. `DEV-007`, `DEV-010`

과거 실행이 있는 드라이브도 목록에서 삭제하지 않되 현재 연결과 이력을 분리한다. 유효한 Worker heartbeat에 원본 identity가 있으면 연결됨, online Worker의 attached volume 목록에서 사라졌으면 연결 해제됨, Worker가 offline이라 관측할 수 없으면 확인 불가다. 연결 해제·확인 불가는 완료 결과와 Catalog를 계속 읽을 수 있지만 새 ArchiveRun 준비·시작은 할 수 없다. 화면의 목록·상세·알림·실행 gate는 5초 polling마다 이 한 projection 값을 함께 바꾼다. `DEV-010`, `DSH-010`, `DSH-026`

원본은 담당자 또는 인증된 Archivist가 현재 candidate ID·revision을 명시적으로 고른 뒤에만 해당 Worker에서 `SOURCE_MEDIA` ReadOnlyStorageRoot로 개방한다. Control은 root 없이 ArchiveIntakeSession의 stable identity와 선택 Actor를 기록하고 선택 storage ID만 heartbeat 응답으로 Worker에 돌려준다. ArchiveIntakeSession은 불변 이력이므로 새 작업 생성 projection은 current candidate revision·Worker·source Storage와 모두 일치하는 세션을 별도 선택 세션 ID로 제공한다. 완료된 과거 ArchiveRun의 접수 세션 ID는 실행 추적에 남지만 새 ArchiveRun 준비에는 쓰지 않는다. 아카이브 저장소는 같은 Worker가 접근할 수 있는 `ARCHIVE_PRIMARY`이며 원본과 다른 논리 Volume이어야 한다. 새 외장 Volume은 원본용 Storage ID와 별도의 archive Storage ID로 후보에 나타나므로 역할이 섞이지 않는다. Worker projection은 로컬 root, 연결된 네트워크 공유, 원본과 다른 새 외장 Volume, 검증된 FTP·SFTP를 `StorageAccessKind`로 구분한다. 선택 전에는 새 외장 Volume에 쓰지 않는다. MCP 후보 응답에는 절대경로·endpoint·credential을 포함하지 않고 현재 접근 가능한 ID·revision·표시명·용량만 포함한다. 접근 불가 원격 서비스는 시작을 막는다. 구현된 용량 admission은 선택 범위 예상 출력에 5%와 64 MiB 중 큰 안전 여유를 더하고, 현재 여유가 필요 용량보다 작으면 Storage ID와 byte 수를 가진 구조화 오류로 실행 생성을 막는다. 패키지와 ArchiveCatalog 게시 잠금은 파일 교체를 직렬화하지만 ArchiveRun 전체 기간의 Storage 용량·쓰기 예약은 아니다. Storage 단위 실행 예약은 `ARC-010` planned이며 구현 전에는 동시에 시작한 실행 둘이 같은 여유 공간을 각각 admission할 수 있다. 정책의 무인 선택과 임의 경로 선택은 허용하지 않는다. `DEV-008`, `DEV-010`, `ARC-010`, `ARC-019`, `APP-006`, `CFG-012~013`, `MCP-003`

Worker는 현재 연결된 외장 Volume의 두 역할 ID를 로컬 `StorageRoot` capability로만 해석하고 root 경로를 Control에 보내지 않는다. capability를 여는 것만으로 파일을 읽거나 쓰지 않으며, 담당자가 선택한 뒤 생성된 fenced 실행 명세만 archive ID에 게시할 수 있다. 분리 시 실행 중 handle을 닫지 않고 유휴 조정에서 동적 capability를 제거한다. `DEV-013`

폴더 탐색과 FFprobe 검사는 원본에 접근하는 Worker에서 실행한다. 선택 직후 folder inventory는 background에서 실행해 대용량 탐색 중에도 heartbeat와 자원 수용 판정을 멈추지 않는다. 지원 regular file만 즉시 상위 정규화 상대 폴더로 묶고, 보호·숨김 항목, symlink, junction·reparse 후보, irregular entry와 root 밖 대상은 따라가지 않는다. 실행 전 bounded heartbeat는 `FolderDiscoveryState`와 revision 집계만 보내고, 크기가 늘어나는 폴더·영상 inventory는 같은 snapshot ID의 별도 요청으로 보낸다. `SCANNING` 부분 집계는 작업 생성 화면에 점진적으로 표시하지만 선택 범위로 고정하지 않는다. `READY` 결과는 0이 아닌 전체 탐색 revision과 영상별 root-relative 경로·파일명·크기·filesystem 수정 시각을 포함하며 드라이브 상세의 폴더 구조에서 펼쳐 본다. `FAILED`는 빈 결과와 구분한다. 아카이브 실행 검사는 각 영상의 정규화한 기술정보를 추가한다. Control에는 Worker의 절대경로와 executable 경로를 보내지 않는다. `DEV-010~011`, `WRK-006`, `DSH-022`, `DSH-033`, `ORG-001`, `CFG-011`, `ORG-010`

탐색 중 원본 Volume이 분리되거나 선택이 해제되면 Worker는 이전 scan context와 부분 inventory를 취소·제거한다. 같은 drive letter를 다른 Volume이 이어받아도 이전 scan은 늦은 batch를 게시하지 않고 새 stable Storage ID가 별도 탐색을 소유한다. 부분 snapshot의 전체 목록 복사는 첫 결과 뒤 기하급수적 간격으로 제한하고 완료 inventory는 생략하지 않는다. `DEV-014`

탐색 결과는 단조 revision을 가진다. 일부 선택과 전체 선택 모두 시작 시점 revision의 폴더 ID 집합으로 고정한다. 이후 파일 추가·삭제가 감지되면 오래된 시작·override를 거부하고 원본 드라이브 상태를 “내용 바뀜”으로 투영한다. `ORG-011`, `APP-002`

완료된 ArchiveRun은 최신 탐색 목록을 소비하지 않는다. 담당자는 같은 원본 revision의 폴더를 새 ArchiveRun ID로 다시 선택할 수 있다. 이때 checksum·상대경로가 같은 입력의 Observation과 OriginalAsset identity는 재사용하지만 새 검사는 새 MediaInspection으로 기록하고 OrganizationDecision·Folder Job·예상 출력은 새 ArchiveRun identity를 포함해 이전 실행과 분리한다. 최초 시작 멱등성 key는 ArchiveRun에 고정하며 같은 ArchiveRun ID와 같은 key의 재전송만 기존 receipt를 반환하고 다른 key는 충돌로 거부한다. `APP-002`, `ORG-012`

## 3. 규칙 기반 분류

DATA 18의 영상·비영상 조사 범위와 분류 근거는 [현장 사례 전체](16-source-classification-cases.md)에 있다. 정책 v3의 폴더 구조, 파일별 근거, 역할 미상 처리, ML 비교 계획은 [분류 정책 설계](17-source-classification-policy.md)에 둔다. 새 실행에서 `ORG-019~026`의 탐색·분류·조회 계약을 적용한다.

기존 정책 v2는 고정된 탐색 리비전의 상대 폴더·파일명과 Worker의 MediaInspection을 사용한다. 발견 단계는 원본 바이트를 읽지 않고 역할 후보와 근거를 제공한다. 선택된 범위를 검사한 뒤 OrganizationDecision이 실행 여부와 근거를 기록한다. v1 결정은 기록 당시의 의미를 유지한다.

- 폴더는 영상의 직접 상위 경로다. 같은 행사 아래 VJ·PGM·고정캠을 분리하고, 영상이 있는 부모와 자식은 각각 처리한다. VJ 또는 소스촬영 표기와 카메라 파일명의 조합을 우선한다. 짧고 호환되며 번호가 연속인 전체 컷은 20Mbps 하나와 컷 사이 마커로 게시한다. PGM·CLN·클린본·녹화 역할이 명확한 단일 입력은 6Mbps로 처리한다. 여러 녹화 파일은 채널·분할 경계를 확인한다. 고정캠·AJA 단독, 편집 결과·참고자료·선별본, 행사 미확인 형식별 폴더와 역할 충돌은 자동 렌더를 보류한다. 명명 관례가 없는 입력에는 기존 기술 판정이 적용된다. 결과는 선택한 ARCHIVE_PRIMARY의 기존 패키지 위치에 게시한다. `ORG-002`, `ORG-016`
- 같은 완료 목록에서 `M4ROOT/SUB/C0001S03.MP4`는 같은 카드의 `CLIP/C0001.MP4`와 번호가 대응하고 크기가 양수이며 더 작을 때 프록시 후보가 된다. 작업 공간에 파일별 원본 상대경로와 대응·미확인 수를 표시한다. 부분 목록이나 원본 없는 SUB는 미확인으로 보존한다. SUB는 자동 렌더하지 않는다. 실행 결정은 선택 범위에서 함께 검사된 원본만 대조한다. `ORG-017`
- 보류 상세에서 180초 이하·1920×1080 이상·영상 스트림 25Mbps 이상은 촬영컷 검토 단서로 제공한다. 1,200초 이상·1280×720 이상·오디오 스트림 존재는 장시간 녹화 검토 단서다. 전체 컨테이너 비트레이트로 누락된 영상 비트레이트를 대체하지 않는다. 해상도·시간은 입력 측정값이며 재생 내용이나 행사 완전성을 증명하지 않는다. 촬영 경로와 복수 카메라 파일명이 원본을 시사할 때 전부 짧은 고비트레이트 입력이고 번호·기술 규격도 일치하는 경우만 20Mbps 자동 합본으로 이어진다. 행사 미확인 형식별 폴더는 파일별 후보가 있어도 순서를 확인할 때까지 보류한다. `ORG-018`

2026-11-23~24 Windows 실측에서 원본 드라이브 한 대의 기존 역할 보류 6,196개를 FFprobe로 재생·디코딩 없이 조사했다. 탐색 revision `4740278653254714`에서 촬영컷 단서는 4,149개, 장시간 녹화 단서는 424개, 단서 부족은 1,610개, 재검사 후에도 헤더를 읽지 못한 파일은 13개였다. 명시적인 폴더 역할을 가진 비교군에서 VJ 60개 중 50개, 녹화 23개 중 17개가 각 기술 단서와 일치했다. 비교 기준은 폴더 역할이고 콘텐츠 정답 라벨은 없다. 행사 미확인 형식별 폴더에서 나온 후보 4,333개는 행사·순서 확인 대상으로 남는다.

| 판정 | 조건 | 1단계 결과 |
|---|---|---|
| `ORGANIZE_AS_IS` | 1080p와 승인 bitrate·codec 범위에 이미 맞음 | 검증 뒤 그대로 보관 |
| `RENDER_RECORDINGS` | 일반 녹화영상이며 새 렌더 필요 | 기본 1080p 6Mbps, 명시 선택 시 20Mbps |
| `COMPILE_SOURCE_SEQUENCE` | 짧은 여러 Clip의 소스 폴더 | 입력 순서·CutMarker를 고정해 1080p 20Mbps 통합 |
| `HOLD` | 하나의 결정으로 수렴하지 않음 | `WAITING/INPUT`, 외부 실행 없음 |

HOLD는 Control 원장과 아카이브 작업 공간에 보류 사유·원본 기술정보와 함께 남지만 layout v3 루트 ArchiveCatalog에는 행을 만들지 않는다. layout v3의 행 identity는 게시된 ArchivePackage이므로 HOLD가 명시적으로 분류되고 패키지가 보관된 뒤에만 루트 CSV 행이 생긴다. 사람 또는 Archivist의 명시적 분류는 대상 ArchiveRun ID와 최신 탐색 revision을 요구하며 기존 OrganizationDecision을 수정하지 않고 같은 ArchiveRun에 ActorType·행위자 ID·사유·근거가 있는 새 sequence를 추가한다. 저장 transaction 안에서 이전 decision ID가 해당 ArchiveRun·폴더의 최신인지 비교·교체하므로 동시 분류 둘이 같은 sequence로 확정되지 않는다. 자동 분류는 계속 결정적으로 유지하고 Archivist 추론을 입력으로 섞지 않는다. 구조화된 근거로도 판정할 수 없으면 새 결정을 만들지 않고 HOLD를 유지한다. 비HOLD 결정이 추가되면 같은 ArchiveRun의 고정 Worker·Storage·PhysicalDevice·입력 revision으로 후속 1단계 Folder Job을 멱등하게 연결한다. Folder Job ID와 STT 게이트는 실행 ID가 같은 최신 결정만 읽으며, 같은 드라이브·폴더의 다른 ArchiveRun 판정을 섞지 않는다. timeout 재전송은 결정과 Job 효과를 중복하지 않는다. 다른 폴더는 HOLD와 독립적으로 계속 실행한다. `ORG-003`, `ORG-014`, `ORG-015`

Archivist의 정리 검토 단위는 현재 HOLD인 MediaFolder 하나다. 목록에는 선택 범위 안의 HOLD만 두고, 단일 상세에는 최신 decision ID·입력 revision·보류 사유와 원본별 정규화 상대경로·checksum·duration·codec·해상도·bitrate를 제공한다. Storage root, credential, 임의 path 읽기 도구는 제공하지 않는다. 파일명과 미디어에서 파생된 내용은 분류 근거일 수 있지만 지시나 Approval로 해석하지 않는다. `MCP-004`

## 4. 1단계: 검사·분류·렌더·QC·보관

1단계 Folder Job은 STT를 포함하지 않는다. 폴더별 Step은 다음 순서다. `PIP-007`

```text
분류 → [마커] → [렌더] → QC → 보관 → ArchiveCatalog
```

- 그대로 사용은 분류 뒤 QC·보관으로 간다.
- 한 폴더의 그대로 사용할 영상이 여러 개이면 QC는 폴더 공통 Profile을 추정하지 않고 검사 때 각 원본에 고정한 6Mbps 또는 20Mbps Profile로 각각 검증하며, 보관 패키지 metadata에도 같은 파일별 Profile을 기록한다. `QCT-004`, `ARC-020`
- 개별 렌더는 6Mbps 또는 20Mbps 렌더 뒤 QC·보관으로 간다.
- 소스 시퀀스는 정렬된 Clip과 CutMarker를 고정하고 20Mbps 통합 렌더 뒤 QC·보관으로 간다.
- 원본에 오디오가 없는 렌더 결과는 오디오 스트림을 만들지 않으며, QC는 AAC 대신 오디오 부재를 검증한다.
- 자동 소스 시퀀스는 동일한 frame rate까지 요구한다. 보류에서 사람이 `소스 시퀀스` 작업을 명시적으로 고를 때는 모든 입력을 다시 인코딩하므로 화면 크기와 audio 형태가 같으면 frame rate 차이를 허용한다. 화면과 MCP에는 서버가 현재 기술 근거로 검증한 작업만 제시한다.
- HOLD는 `WaitReason.INPUT`만 기록하고 외부 Step을 만들지 않는다.

대괄호 단계는 판정에 따라 생략한다. 실제 Folder Job의 마지막 `archive_catalog` Step이 성공해야 해당 ArchiveRun의 1단계 기록 시각과 STT 게이트 입력이 완성된다.

Worker는 외부 subprocess에 executable과 argument 배열만 넘기고 StorageRoot 안 임시 출력에 쓴다. QC와 checksum 검증 뒤에만 선택한 ARCHIVE_PRIMARY에 게시한다. 한 벌이 검증되면 `ArchivePackageState.STORED`, 화면의 “보관됨”이다. 아직 Transcript·Subtitle을 새 패키지 리비전으로 더할 수 있다. `ORG-008`, `ARC-011`

손상 입력·지원하지 않는 codec은 해당 영상만 실패로 종결하고 ArchiveCatalog에 실패 상태를 남긴다. 일시 process 오류는 재시도 횟수를 소비한다. 장치·네트워크 Storage 불가와 실행 중 아카이브 저장소 용량 부족은 대상 Storage ID가 있는 `WAITING/DEVICE`로 보고되어 사람이 복구할 때까지 횟수를 소비하지 않는다. `DEV-012`, `WRK-015`

## 5. 2단계: STT

STT 시작은 내부적으로 별도 Command다. Control 조정기는 해당 ArchiveRun ID에 속한 최신 판정과 결정적 Folder Job만 투영해 1단계에 실행 중이거나 재시도 예정인 작업이 없을 때 결정적 멱등성 key로 이 Command를 자동 제출한다. 사용자는 영상 정리·보관을 시작한 뒤 두 번째 시작 조작을 하지 않는다. HOLD는 명시적 분류 입력을 기다리므로 남은 실행 가능 작업 수와 2단계 대상에서 제외한다. 다른 ArchiveRun와 시스템 전체 queue는 이 게이트에 포함하지 않고 자원 경합은 Scheduler가 처리한다. 기존 HTTP·MCP 시작 Capability는 재전송·복구 호환 경계로 유지한다. `PIP-008`, `APP-008`, `DSH-006`

대상은 1단계에서 보관됨이 되고 원본 기술정보에 오디오가 있는 `ORGANIZE_AS_IS`·`RENDER_RECORDINGS` 녹화영상뿐이다. 무음 영상, 소스 시퀀스, HOLD, 실패 입력은 제외한다. `ORG-007`

대상 기록물마다 1단계 Folder Job과 분리된 STT Job을 만들며 폴더별 2단계 Step은 다음 순서다. `PIP-009`

```text
오디오 → STT → 자막·텍스트 → 기록물에 추가 → ArchiveCatalog
```

AudioArtifact는 16kHz mono PCM 규격과 provenance를 보존한다. 긴 음성은 무음 경계 우선의 제한 길이 청크로 전사하지만 청크는 별도 기록물이 아니며 결과 segment는 원본 영상의 전역 시간축으로 합친다. Transcript는 정규화한 시간 구간 JSON, Subtitle은 SRT이며 `transcript.txt`는 각 구간의 `HH:MM:SS.mmm` 시작·종료 시각과 text를 시간순으로 기록한 사람이 읽는 UTF-8 sidecar다. 세 파일을 같은 ArchivePackage에 추가할 때 기존 리비전을 고치지 않고 새 manifest digest의 리비전을 게시한다. `TRN-002`, `TRN-006`, `ORG-008`, `ARC-011`

Transcript 게시가 성공하면 Worker는 versioned 결정 규칙으로 구간 길이와 문자 밀도를 평가하고 판정을 transcript Step의 출력 기록에 함께 보낸다. `REVIEW_RECOMMENDED`는 2단계 완료를 막지 않으며 폴더별 확인 필요 항목으로 남는다. 담당자는 원본·전사 텍스트·자막을 대조할 대상을 이 신호로 찾고, 화면은 저장된 사유와 측정값만 표시한다. `TRN-007`, `DSH-018`

## 6. ArchivePackage와 ArchiveCatalog

이전 layout v1 구조는 다음과 같다. 결과 영상 하나가 ArchivePackage 하나를 소유하며 SOURCE_MEDIA 원본 파일은 넣지 않는다. 대신 원본 드라이브 ID, 원래 이름, 상대경로, checksum과 길이를 metadata에 기록한다. layout v1·v2는 읽기·복구 호환 대상으로 유지하지만 현재 Production Worker의 새 게시 기본값은 아래 layout v3다.

```text
<아카이브 저장소>/
├── packages/
│   └── 2026/10/31/
│       └── VP-20261031-0001/
│           ├── <원본 영상명 또는 MediaFolder 이름>.mp4
│           ├── transcript.json
│           ├── transcript.txt
│           ├── subtitle.srt
│           ├── metadata.json
│           ├── files.csv
│           ├── events.jsonl
│           ├── manifest-sha256.txt
│           └── README.md
└── archive-catalog.csv
```

layout v1은 `metadata.json`, `files.csv`, `events.jsonl`, manifest와 README로 DB 없이 복원한다. layout v3는 같은 복구 정보를 `.archive-package.json` 하나에 합친다. 어느 형식에도 Storage 절대 root, mount path, credential과 executable 경로를 넣지 않는다. 실제 제품 import 명령과 실제 패키지 acceptance test는 `ARC-007` planned다.

layout v1 호환 패키지는 처리 날짜로 나뉜 상대경로를 계속 읽고 게시할 수 있다. 결과 영상 파일명은 단일 입력이면 원래 영상명, 여러 컷 통합이면 MediaFolder의 마지막 이름에서 만들며 Windows에서 안전하도록 정규화한다. 패키지 ID는 이름과 별개인 불변 identity로 유지하고, metadata와 ArchiveCatalog에는 원래 이름과 전체 상대경로도 함께 남긴다. 화면은 처리 날짜·결과 영상명을 주 정보로, 패키지 ID를 추적용 보조 정보로 보여준다. 완료된 Folder Job의 `ORGANIZED_MEDIA` 출력은 원본 드라이브의 완료 내역에 ArchiveRun을 넘어 유지한다. ArchiveRun 상태만으로 완료 내역을 만들지 않으며 실제 ArchivePackage 출력과 출력 기록 시각을 기준으로 한다. 완료 내역은 출력의 Storage root를 제외한 Package 상대경로를 보존해 같은 ArchiveRun 상세의 정확한 결과로 이동한다. `ARC-013`, `DSH-023~025`

현재 기본 게시 형식인 layout v3는 저장소 root의 디렉터리 깊이를 결과 폴더 1단계로 고정한다. 각 결과 폴더에는 결과 영상 정확히 하나, `video-info.csv`, 2단계 뒤의 `transcript.txt`·`subtitle.srt`, 숨김 복구 문서 `.archive-package.json`만 둔다. 공개 하위 디렉터리와 README·files·events·manifest·current·revision-qualified 파일은 만들지 않는다. 폴더 이름은 처리일과 원본 영상 또는 MediaFolder 이름, package ID suffix로 만들되 identity는 복구 문서의 package ID다. 기존 layout v1·v2는 읽기 호환을 유지하고 자동 이동하지 않는다. `ARC-015`

확정 전 2단계 파일 추가는 같은 결과 폴더에 새 리비전을 게시한다. 현재 영상·텍스트·자막은 사람이 보는 projection이고, 구조화 Transcript와 이전 revision의 metadata·checksum·event 의미·변경된 텍스트 bytes는 `.archive-package.json`의 `revisions[]`에 보존한다. 검증된 공개 projection을 게시한 뒤 이 복구 문서를 마지막에 원자 교체한다. `ARC-016`

layout v1 ArchiveCatalog는 아카이브 저장소 최상단의 `archive-catalog.csv` 하나이며 원본 영상 하나당 한 행이다. 기존 파일은 읽기·복구 호환을 위해 유지한다.

layout v3 ArchiveCatalog는 결과 폴더·ArchivePackage 하나당 한 행이다. 다른 ArchiveRun이나 원본 드라이브라도 같은 `ARCHIVE_PRIMARY`를 대상으로 하면 이 파일에 이어서 기록한다. 행 identity는 package ID이고 `archive_date | result_title`을 인접한 앞 열에 두며 날짜를 제목과 합친 값으로 저장하지 않는다. 뒤에는 layout version, 결과 폴더·영상 상대경로, package revision·manifest digest·상태·판정·Profile·원본 수·구조화 Transcript가 든 복구 문서·TranscriptText·Subtitle 상대경로와 완료 시각을 두고 추적용 ID와 checksum은 마지막에 둔다. 값이 관찰되지 않은 메타데이터는 추정하지 않고 빈 칸으로 남긴다. `ORG-015`

각 결과 폴더의 `video-info.csv`는 OriginalAsset 하나당 한 행이다. `원본 생성 시각 | 원본 영상명`을 인접한 앞 열에 두고 원본 상대경로·checksum·길이·크기·컨테이너·전체 bitrate·video/audio stream 규격·판정 ID를 기록한다. 다중 소스 결과는 이 파일에 N행을 쓰되 루트 ArchiveCatalog에는 패키지 행 하나만 쓴다. `.archive-package.json`은 package identity·layout version·현재 revision·provenance와 복구 계약을 소유하고 검색 projection을 중복 소유하지 않는다. `MED-008`

layout v1의 상태 후보는 분류됨·보류·처리 중·보관됨·STT 완료·확정됨·실패다. layout v3의 루트 행은 ArchivePackage가 보관된 뒤 처음 생기므로 보관됨·STT 완료·확정됨만 투영하고, package가 생기기 전의 HOLD·처리 중·실패는 Control 원장과 작업 공간에 남긴다. package 보관과 새 리비전 게시 때 StorageRoot의 배타 잠금을 잡고 기존 행과 새 행을 병합한 전체 현재 투영을 `archive-catalog.csv.partial`에 쓰고 파일 동기화·검증한 뒤 root-relative rename으로 원자 교체한다. 기존 layout v1·v2 카탈로그는 migration 입력으로 보존하고 v3 행과 암묵적으로 섞지 않는다. `ORG-009`, `MED-007`, `ORG-015`

폴더와 CSV는 전용 대조 operation으로 검증한다. `VERIFY_ARCHIVE_CATALOG`는 root의 1단계 결과 폴더를 읽어 layout version별 복구 문서와 checksum, `결과 폴더 1 = package ID 1 = 결과 영상 1 = 루트 CSV 행 1`, `video-info.csv`의 원본 수를 양방향으로 확인한다. `REBUILD_ARCHIVE_CATALOG`는 유효한 versioned 복구 문서에서 전체 CSV를 임시 생성하고 같은 검증을 통과한 뒤 원자 교체하며 package 파일을 고치거나 삭제하지 않는다. 사람은 `archive-media catalog verify|rebuild`로 직접 실행할 수 있고 Archivist는 임의 shell 대신 MCP Capability로 Control에 요청한다. `ARC-017~018`

아카이브 작업 공간의 ArchiveCatalog 마지막 기록 시각은 해당 ArchiveRun에서 성공 완료된 1·2단계 `archive_catalog` Step 중 가장 최근의 영속 완료 시각이다. 실행 중이거나 실패한 게시 시도는 성공 기록 시각을 앞당기지 않는다. `DSH-027`

“보관됨”은 ARCHIVE_PRIMARY 한 벌의 검증 게시다. “확정됨”은 현재 패키지 리비전이 `ARC-004`의 QC·sidecar·manifest·서로 다른 물리 장치의 두 ArchiveCopy·사람 Approval 조건을 모두 만족한 상태다. 확정 뒤 같은 패키지 리비전을 고치지 않는다. `DSH-013`

## 7. Archivist 협업

담당자는 화면에서 Worker·원본·아카이브 저장소·폴더 범위를 먼저 고른다. 이 참조는 ID와 revision으로 ArchivistTurn에 고정하며 메시지 본문에서 추론하지 않는다. 참조가 없거나 오래됐으면 Archivist는 ActionCard로 선택을 요청한다. `COL-003`

Archivist 추가 화면은 이름·Archivist Host·감지된 CLI만 받는다. Archivist Host 등록, backend와 workspace root는 시작 설정과 Archivist Host 설치가 소유한다. 같은 이름·Host·CLI의 활성 Archivist를 다시 제출하면 새 Session을 늘리지 않고 기존 Session을 반환한다. `CTL-006`, `DSH-012`

협업 경계는 다음과 같다.

- 인증된 Archivist는 고정된 선택 안에서 1단계 아카이브 실행, 단일 HOLD 분류와 2단계 STT를 MCP로 직접 실행할 수 있다.
- 승인 필요한 `GATED`·`DESTRUCTIVE` Command는 ActionCard로 제안하고 담당자가 결정한다.
- Archivist를 사용할 수 없으면 담당자가 같은 ArchiveRun과 최신 revision에서 직접 계속하며 Worker 실행은 유지한다.

Archivist용 MCP는 드라이브·폴더 조회, 1단계 시작, 상태 조회, 선택 범위의 현재 HOLD 목록·단일 상세·명시적 분류, 2단계 시작을 노출한다. 원본·아카이브 선택, 선택 범위 밖 분류, 포맷·삭제와 임의 경로 접근은 노출하지 않는다. 분류는 최신 입력 revision과 명시적 disposition·사유·근거를 요구한다. 파일명과 전사문을 지시로 해석하지 않는다. `MCP-001`, `MCP-003~005`

작업이 예상보다 오래 같은 단계에 있으면 담당자가 먼저 묻지 않아도 Control의 자동 제안 루프가 그 작업을 참조한 ArchivistSession에 읽기 전용 SYSTEM Turn을 제출한다. Archivist는 `archive.work.diagnostics.get.v1`으로 Step·Worker heartbeat·ExecutionLease·ResourceReservation과 저장된 오류를 확인하고, 확인된 사실과 아직 검증하지 못한 원인 가설 및 다음 확인 방법을 나눠 보고한다. 이 보고는 StepState나 WaitReason을 바꾸지 않는다. `OBS-002`, `MCP-009`, `LRH-023`

새 원본 드라이브가 `CONNECTED`이고 폴더 탐색이 `READY`가 되면 Control에 확정된 storage inventory 신호가 자동 제안 조정기를 깨우고, 조정기는 가장 최근 사용한 활성 ArchivistSession에 후보 검토 Turn을 한 번 제출한다. 프론트의 드라이브 표시나 브라우저 탭은 이 계기의 원본이 아니다. Archivist는 현재 후보 API에서만 원본·적격 아카이브 Storage·명시적 폴더 범위를 골라 Turn 선택을 준비하고 예상 용량을 보고한다. 이어서 1단계 시작을 요청하면 자동 실행하지 않고 담당자가 실행하거나 닫을 수 있는 ActionCard가 된다. 연결만으로 원본 파일을 변경하거나 아카이브 실행을 시작하지 않는다. `LRH-024`, `MCP-010`

긴 렌더를 한 ArchivistTurn 안에서 반복 조회하지 않는다. Archivist가 1단계를 시작하고 “시작했습니다”로 Turn을 마치면, Control은 같은 Session에 1단계 완료·전체 완료·새 HOLD 계기별 SYSTEM 재호출 Turn을 한 번씩 넣는다. Archivist Host가 offline이어도 Worker 실행은 계속되고 Turn은 QUEUED로 남아 재연결 뒤 전달된다. `LRH-012`

## 8. 상태와 장애 격리

상태는 서로 추론하지 않고 세 층으로 투영한다.

| 층 | 상태 |
|---|---|
| 원본 드라이브 | 미분류, 진행 중, 보류 있음, 완료, 내용 바뀜 |
| 아카이브 실행 | 1단계 진행, STT 가능, 2단계 진행, 완료 |
| 기록물 | 보관됨, 확정됨 |

원본 드라이브 상태는 stable 논리 Volume identity와 최신 탐색 revision 및 아카이브 실행 이력에서 계산한다. 같은 PhysicalDevice의 다른 Volume은 별도 드라이브로 투영하며 PhysicalDevice identity는 I/O 충돌 키로만 유지한다. 해당 ArchiveRun에 실행 가능한 1·2단계 작업이 없으면 완료로 보고하되 HOLD 수와 명시적 분류 필요 상태는 별도 필드로 유지한다. HOLD가 해소되면 같은 ArchiveRun에 후속 작업을 붙이고 다시 진행 상태가 될 수 있다.

한 폴더나 영상의 실패·HOLD는 다른 폴더를 막지 않는다. 원본 분리와 네트워크 Storage 끊김은 해당 Step만 `DEVICE`로 기다린다. WaitReason이 없는 상태에서 사람·자원 대기를 추론하지 않는다.

## 9. 아카이브 작업 공간

아카이브 작업 공간의 화면 계약은 [09. 아카이브 작업 공간](09-archive-workspace.md) §7을 따른다. 왼쪽 사이드바 메뉴는 작업 생성·작업 관리·장비·에이전트 순서이고 작업 생성은 최상단, 작업 관리는 기본 화면이며, Archivist 대화 칸은 모든 화면의 오른쪽에 늘 있고 화면에는 에이전트로 표기한다. `DSH-004`, `DSH-007`, `DSH-009`, `DSH-019`, `DSH-025`

- 상태: 모든 드라이브·Worker의 실패와 사람 대기를 묶지 않고 문제 하나마다 카드로 보이고, 카드를 누르면 그 문제를 펼친 드라이브 상세로 이동한다. 지금 하는 일은 처리 중인 드라이브의 단계 상태 행렬이다.
- 드라이브 목록: 가져올 드라이브를 아직 처리 안 함·처리 중·최근 작업 완료로 묶고 작업 PC·결과 저장 위치·단계·저장 영상 진행·실패와 직접 확인할 항목 수를 한 줄로 보인다.
- 드라이브 상세: 연결 상태, 현재 폴더 구조와 폴더를 펼쳤을 때의 영상 기록, ArchiveRun을 넘어 유지하는 저장된 결과, 기존 실행의 직접 확인할 항목과 단계 상태를 둔다. 새 영상 정리·보관 작업의 저장 위치·범위 선택과 시작은 두지 않는다. 저장된 결과는 실제 저장된 결과 영상명·처리 방식·원본 수·완료 시각·결과 저장 위치·저장 상태를 최신 순서로 보이고 행을 누르면 해당 ArchiveRun 상세로 이동한다.
- 작업 생성: 영상 정리·보관과 기타 작업을 구분하고 생성 입력만 표시한다. 영상 정리·보관은 가져올 드라이브, 그 Worker가 접근하는 저장할 위치, READY 폴더 범위와 예상 결과 위치·용량·시작 불가 이유를 확인한 뒤 ArchiveRun 하나를 만들고 그 상세로 이동한다. 예상 결과 위치는 layout v3의 root-relative 날짜·원본 기반 1-level 폴더 패턴과 결과 수 범위, 파일 구성, 안전 여유를 포함한 필요·현재 여유를 보이되 분류 전 Package suffix는 placeholder이고 확정 경로나 예약이 아니다.
- 작업 관리: 생성 입력 없이 진행 중, 시작 대기, 직접 확인할 항목, 완료된 작업 목록을 둔다. ArchiveRun 상세는 하위 Job과 ArchivePackage 저장된 결과를 함께 본다. `DSH-019`, `DSH-043`
- 작업 PC: 처리 중인 드라이브, 드라이브에 속하지 않은 Job, 결과 저장 위치 접근과 자원을 보인다.

자막·텍스트는 영상 정리·저장 게이트 뒤 자동으로 이어지며 별도 시작 버튼이나 ActionCard를 두지 않는다. 드라이브 단계는 `실행 전 → 영상 정리·저장 중 → 자막·텍스트 자동 시작 중 → 자막·텍스트 만드는 중 → 작업 완료` 순서이고, 폴더 단계 순서 `처리 방식 결정·영상 구간 정리·영상 변환·결과 검사·결과 저장·영상 목록 갱신 | 음성 준비·음성을 텍스트로 변환·자막 생성·텍스트 파일 생성·결과에 추가·영상 목록 갱신`은 폴더 행의 단계 점과 펼친 처리 과정에서만 보인다. 단계 점과 폴더 행은 영속 StepState와 저장된 WaitReason만 사용한다. `INPUT`·`APPROVAL`·`DEVICE`는 직접 확인할 항목과 폴더 행 글자, `MEMORY`·`SCRATCH`·`SLOT`·`DISK`·`IDLE`·`PRIORITY`는 진행 중과 세부의 이유 label로 표시한다. `DSH-006`, `DSH-008`

화면은 기본 어두운·선택 밝은 무채색 표면, 목록 hairline, 2px 이하 모서리와 명사형 label을 사용하고 설명형 본문을 두지 않는다. Darwin·Linux·Windows Chromium PNG golden과 폭별 배치 검사로 승인한다. `DSH-004`, `DSH-007`, `DSH-009`

## 10. Worker operation 경계

Archive Intake를 위해 Worker에는 최소 다음 typed operation이 필요하다.

```text
DISCOVER_MEDIA_FOLDERS
INSPECT_MEDIA
RENDER_RECORDING
COMPILE_SOURCE_SEQUENCE
RUN_MEDIA_QC
PUBLISH_ARCHIVE_PACKAGE
EXTRACT_AUDIO
TRANSCRIBE_AUDIO
CREATE_SUBTITLE
REVISE_ARCHIVE_PACKAGE
PUBLISH_ARCHIVE_CATALOG
VERIFY_ARCHIVE_CATALOG
REBUILD_ARCHIVE_CATALOG
```

Control의 StepExecutionSpec은 Storage ID, 정규화 상대경로, checksum, profile reference와 예상 출력 ID만 보낸다. Worker는 실행 파일·로컬/UNC root·원격 endpoint·credential reference를 로컬 strict 설정에서 해석한다. FTP·SFTP Adapter는 임시 업로드와 다시 읽기, 목록, manifest checksum 및 원자 게시 또는 동등한 안전 게시를 검증해야 접근 가능으로 보고한다. 실제 Windows 외장 volume 관찰·사람 선택·비동기 folder inventory와 별도 1·2단계 렌더·QC·STT·패키지 실행은 실장비 수직 검증을 통과했다. 영상별 Worker FFprobe 원격 검사, 네트워크 공유와 FTP·SFTP Storage는 각 acceptance가 통과하기 전까지 `planned`다. `CFG-011~013`, `ORG-010`, `APP-007`

## 11. v1 결정

| ID | 결정할 내용 | 확정안 |
|---|---|---|
| Q1 | 아카이브 실행 개체와 정식 이름 | **결정: 기존 run과 분리한 영속 `ArchiveRun`** |
| Q2 | STT 게이트가 보는 queue 범위 | 해당 아카이브 실행의 1단계만 |
| Q3 | STT Job 모델 | 별도 STT Job |
| Q4 | ArchiveCatalog 기록 방식 | 전체 임시 작성·검증·원자 교체 |
| Q5 | 패키지 단위와 SOURCE_MEDIA 원본 포함 | 결과 영상 하나당 하나, 원본 파일 제외·추적 metadata 포함 |
| Q6 | 실행 중 아카이브 저장소 용량 부족 | 사람 조치 대기, `WaitReason.DEVICE` |
| Q7 | 긴 작업 뒤 Archivist 재개 방식 | 같은 Session의 SYSTEM 재호출 Turn |
| Q8 | Archivist 추가 화면 범위 | Archivist 생성만, Archivist Host는 설정 파일·Host 설치 |
| Q9 | 사람이 읽는 내용 텍스트 파일 | UTF-8 `transcript.txt` 포함 |
| Q10 | HOLD가 남은 아카이브 실행의 완료 의미 | 실행 가능 작업이 없으면 완료 보고, HOLD 별도 표시 |
| Q11 | 네트워크 Storage identity·접근 | UNC root·Secret Store reference·루트 식별 파일 |
| Q12 | DEVICE 대기의 화면 분류 | 사람 조치 대기로 통일 |
| Q13 | 새 ArchivePackage 디렉터리 깊이 | layout v2부터 Storage root 바로 아래 결과 폴더 1단계, layout v3는 공개 파일을 최소화하고 복구 기록을 숨김 문서 하나로 통합 |
| Q14 | 폴더와 CSV의 행 단위 | 루트 ArchiveCatalog는 Package별 1행, 폴더 `video-info.csv`는 OriginalAsset별 N행 |
| Q15 | 폴더와 ArchiveCatalog 대조 | typed `verify`와 검증 뒤 원자 교체하는 `rebuild`, package는 수정·삭제하지 않음 |

Q1~Q12는 사용자의 “테스트 버전까지 권장안으로 진행” 결정에 따라 2026-10-31에 확정했다. Q13~Q15는 2026-11-09에 확정했고 layout v2 writer·대조·재생성을 typed MediaOperation과 CLI에 연결해 integration test로 검증했다. 2026-11-12에는 layout v3 compact writer와 v2·v3 대조·재생성을 추가했다. 기존 layout v1·v2 기록은 자동 migration하지 않는다.

### 실패한 QC의 인수

측정 결과가 기준을 벗어나면 실패 판단을 작업 원장으로 보낸다. 평가 ID, 검사 대상 ID, 원본 검사 또는 타임라인 ID와 실패한 항목별 기대값·실측값을 보존한다. 길이 검사는 허용차도 제공한다. 원본 Job에서 실패 증적 ID로 상세를 조회해 어떤 입력이나 프로필을 확인할지 결정할 수 있다. 검사를 실행할 수 없었던 도구 장애는 독립적으로 분류한다. `QCT-005`
