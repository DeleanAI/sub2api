<template>
  <div class="card" data-testid="custom-pages-editor">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <div class="flex flex-col gap-3 lg:flex-row lg:items-start lg:justify-between">
        <div>
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
            {{ t("admin.settings.customPages.title") }}
          </h2>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.customPages.description") }}
          </p>
          <p v-if="limits" class="mt-1 text-xs text-gray-400 dark:text-gray-500">
            {{
              t("admin.settings.customPages.limitsHint", {
                page: formatBytes(limits.max_content_size, 0),
                asset: formatBytes(limits.max_asset_size, 0),
                count: limits.max_assets,
              })
            }}
          </p>
        </div>
        <button
          type="button"
          class="btn btn-secondary btn-sm"
          :disabled="loading"
          @click="loadPages"
        >
          {{ t("admin.settings.customPages.refresh") }}
        </button>
      </div>
    </div>

    <div class="grid grid-cols-1 gap-6 p-6 lg:grid-cols-[260px_1fr]">
      <!-- Page list -->
      <div class="space-y-3">
        <h3 class="text-sm font-semibold text-gray-700 dark:text-gray-300">
          {{ t("admin.settings.customPages.pages") }}
        </h3>
        <div v-if="loading" class="py-6 text-center">
          <div
            class="mx-auto h-6 w-6 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"
          ></div>
        </div>
        <p
          v-else-if="pages.length === 0"
          class="text-sm text-gray-500 dark:text-gray-400"
          data-testid="custom-pages-empty"
        >
          {{ t("admin.settings.customPages.noPages") }}
        </p>
        <ul v-else class="space-y-1" data-testid="custom-pages-list">
          <li v-for="page in pages" :key="page.slug">
            <button
              type="button"
              class="w-full rounded-lg border px-3 py-2 text-left text-sm transition-colors"
              :class="
                page.slug === selectedSlug
                  ? 'border-primary-400 bg-primary-50 text-primary-700 dark:border-primary-500 dark:bg-primary-900/20 dark:text-primary-300'
                  : 'border-gray-200 text-gray-700 hover:bg-gray-50 dark:border-dark-600 dark:text-gray-300 dark:hover:bg-dark-700'
              "
              :data-testid="`custom-page-item-${page.slug}`"
              @click="selectPage(page.slug)"
            >
              <span class="block truncate font-mono">{{ page.slug }}</span>
              <span class="block text-xs text-gray-400 dark:text-gray-500">
                {{ formatBytes(page.content_size) }} ·
                {{ t("admin.settings.customPages.assetCount", { count: page.asset_count }) }}
              </span>
            </button>
          </li>
        </ul>

        <div class="space-y-2 border-t border-gray-100 pt-3 dark:border-dark-700">
          <label class="block text-xs font-medium text-gray-600 dark:text-gray-400">
            {{ t("admin.settings.customPages.newSlug") }}
          </label>
          <input
            v-model="newSlug"
            type="text"
            class="input font-mono text-sm"
            :placeholder="t('admin.settings.customPages.newSlugPlaceholder')"
            data-testid="custom-pages-new-slug"
            @keydown.enter.prevent="createPage"
          />
          <button
            type="button"
            class="btn btn-primary btn-sm w-full"
            :disabled="!newSlug.trim() || saving"
            data-testid="custom-pages-create"
            @click="createPage"
          >
            {{ t("admin.settings.customPages.create") }}
          </button>
        </div>
      </div>

      <!-- Editor -->
      <div v-if="!selectedSlug" class="flex items-center justify-center rounded-lg border border-dashed border-gray-300 p-10 text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400">
        {{ t("admin.settings.customPages.selectHint") }}
      </div>
      <div v-else class="space-y-5" data-testid="custom-pages-detail">
        <div class="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
          <div class="min-w-0">
            <p class="truncate font-mono text-sm font-semibold text-gray-900 dark:text-white">
              {{ selectedSlug }}
            </p>
            <p class="text-xs text-gray-500 dark:text-gray-400">
              {{ t("admin.settings.customPages.menuHint", { url: `md:${selectedSlug}` }) }}
            </p>
          </div>
          <div class="flex items-center gap-2">
            <span
              v-if="dirty"
              class="text-xs text-amber-600 dark:text-amber-400"
              data-testid="custom-pages-unsaved"
            >
              {{ t("admin.settings.customPages.unsaved") }}
            </span>
            <button
              type="button"
              class="btn btn-primary btn-sm"
              :disabled="saving || !dirty"
              data-testid="custom-pages-save"
              @click="savePage"
            >
              {{ t("admin.settings.customPages.save") }}
            </button>
            <button
              type="button"
              class="btn btn-danger btn-sm"
              :disabled="deleting"
              data-testid="custom-pages-delete"
              @click="showDeleteConfirm = true"
            >
              {{ t("admin.settings.customPages.deletePage") }}
            </button>
          </div>
        </div>

        <div class="grid grid-cols-1 gap-4 xl:grid-cols-2">
          <div>
            <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t("admin.settings.customPages.content") }}
            </label>
            <textarea
              v-model="content"
              class="input min-h-[320px] w-full resize-y font-mono text-sm"
              :placeholder="t('admin.settings.customPages.contentPlaceholder')"
              :disabled="detailLoading"
              data-testid="custom-pages-content"
            ></textarea>
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t("admin.settings.customPages.preview") }}
            </label>
            <div
              class="markdown-page-content min-h-[320px] overflow-auto rounded-lg border border-gray-200 bg-white p-4 text-sm dark:border-dark-600 dark:bg-dark-800"
              data-testid="custom-pages-preview"
            >
              <div v-if="preview.html" v-html="preview.html"></div>
              <p v-else class="text-gray-400">{{ t("admin.settings.customPages.previewEmpty") }}</p>
            </div>
          </div>
        </div>

        <!-- Assets -->
        <div class="space-y-3 border-t border-gray-100 pt-4 dark:border-dark-700">
          <h3 class="text-sm font-semibold text-gray-700 dark:text-gray-300">
            {{ t("admin.settings.customPages.assets") }}
          </h3>

          <div class="flex flex-col gap-2 sm:flex-row sm:items-end">
            <div class="flex-1">
              <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
                {{ t("admin.settings.customPages.assetPath") }}
              </label>
              <input
                v-model="uploadPath"
                type="text"
                class="input font-mono text-sm"
                :placeholder="t('admin.settings.customPages.assetPathPlaceholder')"
                data-testid="custom-pages-asset-path"
              />
            </div>
            <div>
              <label class="btn btn-secondary btn-sm cursor-pointer">
                {{ uploadFile ? uploadFile.name : t("admin.settings.customPages.chooseFile") }}
                <input
                  type="file"
                  class="hidden"
                  data-testid="custom-pages-asset-file"
                  @change="onFileChange"
                />
              </label>
            </div>
            <button
              type="button"
              class="btn btn-primary btn-sm"
              :disabled="!uploadFile || uploading"
              data-testid="custom-pages-asset-upload"
              @click="uploadAsset"
            >
              {{ t("admin.settings.customPages.upload") }}
            </button>
          </div>

          <p
            v-if="assets.length === 0"
            class="text-sm text-gray-500 dark:text-gray-400"
            data-testid="custom-pages-assets-empty"
          >
            {{ t("admin.settings.customPages.noAssets") }}
          </p>
          <div v-else class="overflow-x-auto">
            <table class="w-full text-sm" data-testid="custom-pages-assets">
              <thead>
                <tr class="text-left text-xs text-gray-500 dark:text-gray-400">
                  <th class="py-1 pr-3 font-medium">{{ t("admin.settings.customPages.assetPath") }}</th>
                  <th class="py-1 pr-3 font-medium">{{ t("admin.settings.customPages.assetType") }}</th>
                  <th class="py-1 pr-3 font-medium">{{ t("admin.settings.customPages.assetSize") }}</th>
                  <th class="py-1 pr-3 font-medium">{{ t("admin.settings.customPages.assetUpdated") }}</th>
                  <th class="py-1"></th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="asset in assets"
                  :key="asset.path"
                  class="border-t border-gray-100 dark:border-dark-700"
                >
                  <td class="py-1.5 pr-3 font-mono text-xs">{{ asset.path }}</td>
                  <td class="py-1.5 pr-3 text-xs">{{ asset.content_type }}</td>
                  <td class="py-1.5 pr-3 text-xs">{{ formatBytes(asset.size) }}</td>
                  <td class="py-1.5 pr-3 text-xs">{{ formatDateTime(asset.updated_at) }}</td>
                  <td class="py-1.5 text-right">
                    <button
                      type="button"
                      class="rounded p-1 text-gray-400 hover:bg-gray-100 hover:text-gray-600 dark:hover:bg-dark-700"
                      :title="t('admin.settings.customPages.copyReference')"
                      @click="copyReference(asset)"
                    >
                      <Icon name="copy" size="sm" />
                    </button>
                    <button
                      type="button"
                      class="rounded p-1 text-red-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20"
                      :title="t('admin.settings.customPages.deleteAsset')"
                      :data-testid="`custom-pages-asset-delete-${asset.path}`"
                      @click="assetToDelete = asset"
                    >
                      <Icon name="trash" size="sm" />
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </div>

    <ConfirmDialog
      :show="showDeleteConfirm"
      :title="t('admin.settings.customPages.deleteConfirmTitle')"
      :message="t('admin.settings.customPages.deleteConfirmMessage', { slug: selectedSlug })"
      danger
      @confirm="deletePage"
      @cancel="showDeleteConfirm = false"
    />
    <ConfirmDialog
      :show="assetToDelete !== null"
      :title="t('admin.settings.customPages.deleteAssetConfirmTitle')"
      :message="t('admin.settings.customPages.deleteAssetConfirmMessage', { path: assetToDelete?.path ?? '' })"
      danger
      @confirm="deleteAsset"
      @cancel="assetToDelete = null"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { adminAPI } from "@/api";
import type { CustomPageAsset, CustomPageLimits, CustomPageSummary } from "@/api/admin/pages";
import ConfirmDialog from "@/components/common/ConfirmDialog.vue";
import Icon from "@/components/icons/Icon.vue";
import { useAppStore } from "@/stores";
import { useClipboard } from "@/composables/useClipboard";
import { extractApiErrorMessage } from "@/utils/apiError";
import { formatBytes, formatDateTime } from "@/utils/format";
import { renderCustomPageMarkdown } from "@/utils/customPageMarkdown";

const { t } = useI18n();
const appStore = useAppStore();
const { copyToClipboard } = useClipboard();

const loading = ref(false);
const detailLoading = ref(false);
const saving = ref(false);
const deleting = ref(false);
const uploading = ref(false);

const pages = ref<CustomPageSummary[]>([]);
const limits = ref<CustomPageLimits | null>(null);
const selectedSlug = ref("");
const content = ref("");
const savedContent = ref("");
const assets = ref<CustomPageAsset[]>([]);

const newSlug = ref("");
const uploadPath = ref("");
const uploadFile = ref<File | null>(null);
const showDeleteConfirm = ref(false);
const assetToDelete = ref<CustomPageAsset | null>(null);

const dirty = computed(() => content.value !== savedContent.value);

// 预览与用户端走同一套渲染（含相对图片改写），所见即所得。
const preview = computed(() => {
  if (!selectedSlug.value || !content.value.trim()) {
    return { html: "", toc: [] };
  }
  return renderCustomPageMarkdown(selectedSlug.value, content.value);
});

async function loadPages() {
  loading.value = true;
  try {
    const response = await adminAPI.pages.list();
    pages.value = response.pages;
    limits.value = response.limits;
    if (selectedSlug.value && !pages.value.some((page) => page.slug === selectedSlug.value)) {
      selectedSlug.value = "";
      content.value = "";
      savedContent.value = "";
      assets.value = [];
    }
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t("admin.settings.customPages.loadFailed")));
  } finally {
    loading.value = false;
  }
}

async function selectPage(slug: string) {
  selectedSlug.value = slug;
  detailLoading.value = true;
  try {
    const [page, pageAssets] = await Promise.all([adminAPI.pages.get(slug), adminAPI.pages.listAssets(slug)]);
    content.value = page.content;
    savedContent.value = page.content;
    assets.value = pageAssets;
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t("admin.settings.customPages.loadFailed")));
  } finally {
    detailLoading.value = false;
  }
}

async function createPage() {
  const slug = newSlug.value.trim();
  if (!slug || saving.value) return;
  saving.value = true;
  try {
    // 空正文先占位：slug 合法性由后端裁决，前端不复制一份规则。
    await adminAPI.pages.save(slug, "");
    newSlug.value = "";
    await loadPages();
    await selectPage(slug);
    appStore.showSuccess(t("admin.settings.customPages.saveSuccess"));
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t("admin.settings.customPages.saveFailed")));
  } finally {
    saving.value = false;
  }
}

async function savePage() {
  if (!selectedSlug.value || saving.value) return;
  saving.value = true;
  try {
    const page = await adminAPI.pages.save(selectedSlug.value, content.value);
    savedContent.value = page.content;
    content.value = page.content;
    appStore.showSuccess(t("admin.settings.customPages.saveSuccess"));
    await loadPages();
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t("admin.settings.customPages.saveFailed")));
  } finally {
    saving.value = false;
  }
}

async function deletePage() {
  showDeleteConfirm.value = false;
  if (!selectedSlug.value || deleting.value) return;
  deleting.value = true;
  try {
    await adminAPI.pages.remove(selectedSlug.value);
    selectedSlug.value = "";
    content.value = "";
    savedContent.value = "";
    assets.value = [];
    appStore.showSuccess(t("admin.settings.customPages.deleteSuccess"));
    await loadPages();
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t("admin.settings.customPages.deleteFailed")));
  } finally {
    deleting.value = false;
  }
}

function onFileChange(event: Event) {
  const input = event.target as HTMLInputElement;
  uploadFile.value = input.files?.[0] ?? null;
  if (uploadFile.value && !uploadPath.value.trim()) {
    uploadPath.value = uploadFile.value.name;
  }
}

async function uploadAsset() {
  if (!selectedSlug.value || !uploadFile.value || uploading.value) return;
  const path = uploadPath.value.trim() || uploadFile.value.name;
  uploading.value = true;
  try {
    await adminAPI.pages.uploadAsset(selectedSlug.value, path, uploadFile.value);
    uploadFile.value = null;
    uploadPath.value = "";
    assets.value = await adminAPI.pages.listAssets(selectedSlug.value);
    appStore.showSuccess(t("admin.settings.customPages.uploadSuccess"));
    await loadPages();
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t("admin.settings.customPages.uploadFailed")));
  } finally {
    uploading.value = false;
  }
}

async function deleteAsset() {
  const target = assetToDelete.value;
  assetToDelete.value = null;
  if (!selectedSlug.value || !target) return;
  try {
    await adminAPI.pages.deleteAsset(selectedSlug.value, target.path);
    assets.value = await adminAPI.pages.listAssets(selectedSlug.value);
    appStore.showSuccess(t("admin.settings.customPages.deleteAssetSuccess"));
    await loadPages();
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t("admin.settings.customPages.deleteAssetFailed")));
  }
}

async function copyReference(asset: CustomPageAsset) {
  const alt = asset.path.split("/").pop() ?? asset.path;
  await copyToClipboard(`![${alt}](${asset.path})`);
}

onMounted(() => {
  void loadPages();
});
</script>
