import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount } from "@vue/test-utils";

import CustomPagesEditor from "../CustomPagesEditor.vue";

const {
  list,
  get,
  save,
  remove,
  listAssets,
  uploadAsset,
  deleteAsset,
  showError,
  showSuccess,
  copyToClipboard,
} = vi.hoisted(() => ({
  list: vi.fn(),
  get: vi.fn(),
  save: vi.fn(),
  remove: vi.fn(),
  listAssets: vi.fn(),
  uploadAsset: vi.fn(),
  deleteAsset: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
  copyToClipboard: vi.fn(),
}));

vi.mock("@/api", () => ({
  adminAPI: {
    pages: { list, get, save, remove, listAssets, uploadAsset, deleteAsset },
  },
}));

// The shared Markdown renderer only needs buildApiUrl; keep the real axios client (and its i18n) out of the test.
vi.mock("@/api/client", () => ({
  buildApiUrl: (path: string) => `/api/v1${path}`,
  apiClient: {},
}));

// format helpers pull in the real i18n instance; the test only needs deterministic strings.
vi.mock("@/utils/format", () => ({
  formatBytes: (bytes: number) => `${bytes}B`,
  formatDateTime: (value: string) => value,
}));

vi.mock("@/stores", () => ({
  useAppStore: () => ({ showError, showSuccess, showWarning: vi.fn(), showInfo: vi.fn() }),
}));

vi.mock("@/composables/useClipboard", () => ({
  useClipboard: () => ({ copyToClipboard }),
}));

vi.mock("vue-i18n", () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params ? `${key}:${JSON.stringify(params)}` : key,
  }),
}));

const limits = { max_content_size: 1048576, max_asset_size: 5242880, max_assets: 200, max_slug_length: 64 };

const guideSummary = {
  slug: "guide",
  content_size: 7,
  asset_count: 1,
  asset_bytes: 3,
  created_at: "2026-08-22T00:00:00Z",
  updated_at: "2026-08-22T00:00:00Z",
};

const guidePage = {
  slug: "guide",
  content: "# Guide\n![logo](images/logo.png)",
  content_size: 7,
  created_at: "2026-08-22T00:00:00Z",
  updated_at: "2026-08-22T00:00:00Z",
};

const logoAsset = {
  path: "images/logo.png",
  content_type: "image/png",
  size: 3,
  created_at: "2026-08-22T00:00:00Z",
  updated_at: "2026-08-22T00:00:00Z",
};

function mountEditor() {
  return mount(CustomPagesEditor, {
    global: {
      stubs: {
        ConfirmDialog: {
          props: ["show", "title", "message", "danger"],
          emits: ["confirm", "cancel"],
          template:
            '<div v-if="show" data-testid="confirm-dialog"><button data-testid="confirm-yes" @click="$emit(\'confirm\')">yes</button></div>',
        },
        Icon: true,
      },
    },
  });
}

describe("CustomPagesEditor", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    list.mockResolvedValue({ pages: [guideSummary], limits });
    get.mockResolvedValue(guidePage);
    listAssets.mockResolvedValue([logoAsset]);
    save.mockImplementation(async (slug: string, content: string) => ({ ...guidePage, slug, content, content_size: content.length }));
    remove.mockResolvedValue({ message: "page deleted" });
    uploadAsset.mockResolvedValue(logoAsset);
    deleteAsset.mockResolvedValue({ message: "asset deleted" });
  });

  it("lists pages with the backend limits and opens a page for editing with a live preview", async () => {
    const wrapper = mountEditor();
    await flushPromises();

    expect(list).toHaveBeenCalledTimes(1);
    expect(wrapper.find('[data-testid="custom-pages-list"]').text()).toContain("guide");
    expect(wrapper.text()).toContain('"count":200');

    await wrapper.find('[data-testid="custom-page-item-guide"]').trigger("click");
    await flushPromises();

    expect(get).toHaveBeenCalledWith("guide");
    expect(listAssets).toHaveBeenCalledWith("guide");
    const textarea = wrapper.find('[data-testid="custom-pages-content"]');
    expect((textarea.element as HTMLTextAreaElement).value).toBe(guidePage.content);

    // Preview renders through the shared Markdown pipeline: relative image rewritten to the asset endpoint.
    const preview = wrapper.find('[data-testid="custom-pages-preview"]');
    expect(preview.html()).toContain("<h1");
    expect(preview.html()).toContain("/pages/guide/images/images/logo.png");
    expect(wrapper.find('[data-testid="custom-pages-assets"]').text()).toContain("images/logo.png");
  });

  it("saves edited content and reports dirty state", async () => {
    const wrapper = mountEditor();
    await flushPromises();
    await wrapper.find('[data-testid="custom-page-item-guide"]').trigger("click");
    await flushPromises();

    expect(wrapper.find('[data-testid="custom-pages-unsaved"]').exists()).toBe(false);
    await wrapper.find('[data-testid="custom-pages-content"]').setValue("# Changed");
    expect(wrapper.find('[data-testid="custom-pages-unsaved"]').exists()).toBe(true);

    await wrapper.find('[data-testid="custom-pages-save"]').trigger("click");
    await flushPromises();

    expect(save).toHaveBeenCalledWith("guide", "# Changed");
    expect(showSuccess).toHaveBeenCalled();
    expect(wrapper.find('[data-testid="custom-pages-unsaved"]').exists()).toBe(false);
  });

  it("creates a page from the slug input and selects it", async () => {
    list
      .mockResolvedValueOnce({ pages: [], limits })
      .mockResolvedValue({ pages: [{ ...guideSummary, slug: "faq", asset_count: 0 }], limits });
    get.mockResolvedValue({ ...guidePage, slug: "faq", content: "" });
    listAssets.mockResolvedValue([]);

    const wrapper = mountEditor();
    await flushPromises();
    expect(wrapper.find('[data-testid="custom-pages-empty"]').exists()).toBe(true);

    await wrapper.find('[data-testid="custom-pages-new-slug"]').setValue("faq");
    await wrapper.find('[data-testid="custom-pages-create"]').trigger("click");
    await flushPromises();

    expect(save).toHaveBeenCalledWith("faq", "");
    expect(get).toHaveBeenCalledWith("faq");
    expect(wrapper.find('[data-testid="custom-pages-detail"]').text()).toContain("md:faq");
  });

  it("surfaces backend validation errors instead of duplicating the rules client-side", async () => {
    save.mockRejectedValueOnce({ status: 400, code: 400, message: "invalid page slug" });
    const wrapper = mountEditor();
    await flushPromises();

    await wrapper.find('[data-testid="custom-pages-new-slug"]').setValue("bad slug");
    await wrapper.find('[data-testid="custom-pages-create"]').trigger("click");
    await flushPromises();

    expect(showError).toHaveBeenCalledWith("invalid page slug");
  });

  it("uploads an asset under the chosen path and refreshes the asset list", async () => {
    const wrapper = mountEditor();
    await flushPromises();
    await wrapper.find('[data-testid="custom-page-item-guide"]').trigger("click");
    await flushPromises();

    const file = new File(["png"], "banner.png", { type: "image/png" });
    const input = wrapper.find('[data-testid="custom-pages-asset-file"]');
    Object.defineProperty(input.element, "files", { value: [file] });
    await input.trigger("change");

    // Path defaults to the file name and can be overridden.
    expect((wrapper.find('[data-testid="custom-pages-asset-path"]').element as HTMLInputElement).value).toBe("banner.png");
    await wrapper.find('[data-testid="custom-pages-asset-path"]').setValue("images/banner.png");
    await wrapper.find('[data-testid="custom-pages-asset-upload"]').trigger("click");
    await flushPromises();

    expect(uploadAsset).toHaveBeenCalledWith("guide", "images/banner.png", file);
    expect(listAssets).toHaveBeenCalledTimes(2);
    expect(showSuccess).toHaveBeenCalled();
  });

  it("deletes an asset and a page after confirmation", async () => {
    const wrapper = mountEditor();
    await flushPromises();
    await wrapper.find('[data-testid="custom-page-item-guide"]').trigger("click");
    await flushPromises();

    await wrapper.find('[data-testid="custom-pages-asset-delete-images/logo.png"]').trigger("click");
    await wrapper.find('[data-testid="confirm-yes"]').trigger("click");
    await flushPromises();
    expect(deleteAsset).toHaveBeenCalledWith("guide", "images/logo.png");

    await wrapper.find('[data-testid="custom-pages-delete"]').trigger("click");
    await wrapper.find('[data-testid="confirm-yes"]').trigger("click");
    await flushPromises();
    expect(remove).toHaveBeenCalledWith("guide");
    expect(wrapper.find('[data-testid="custom-pages-detail"]').exists()).toBe(false);
  });
});
