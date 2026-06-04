import { createApp, h, ref, nextTick, onMounted, onUnmounted } from 'vue';
import VueOfficeDocx from '@vue-office/docx/lib/v3/vue-office-docx.mjs';
import VueOfficeExcel from '@vue-office/excel/lib/v3/vue-office-excel.mjs';
import VueOfficePptx from '@vue-office/pptx/lib/v3/vue-office-pptx.mjs';

import '@vue-office/docx/lib/v3/index.css';
import '@vue-office/excel/lib/v3/index.css';

let currentApp = null;
let pageObserver = null;

function getDocxPages(container) {
    const wrapper = container.querySelector('.docx-wrapper');
    if (!wrapper) return [];
    const pages = wrapper.querySelectorAll(':scope > section, :scope > .page, :scope > div[class*="page"]');
    return pages.length > 0 ? Array.from(pages) : Array.from(wrapper.children);
}

function getPptxSlides(container) {
    const wrapper = container.querySelector('.pptx-preview-wrapper');
    if (!wrapper) return [];
    return Array.from(wrapper.children);
}

function setupPageObserver(container, pages, callbacks) {
    if (pageObserver) { pageObserver.disconnect(); pageObserver = null; }
    if (!pages.length || !window.IntersectionObserver) return;

    let lastPage = 0;
    pageObserver = new IntersectionObserver((entries) => {
        for (const entry of entries) {
            if (entry.isIntersecting) {
                const idx = pages.indexOf(entry.target);
                if (idx >= 0 && idx !== lastPage) {
                    lastPage = idx;
                    if (callbacks && callbacks.onPageChange) {
                        callbacks.onPageChange(idx + 1);
                    }
                }
            }
        }
    }, { root: container, threshold: 0.5 });

    pages.forEach(p => pageObserver.observe(p));
}

window.VueOffice = {
    render(container, type, arrayBuffer, callbacks) {
        this.destroy();
        const ext = type.toLowerCase();
        const componentMap = { docx: VueOfficeDocx, xlsx: VueOfficeExcel, xls: VueOfficeExcel, pptx: VueOfficePptx };
        const component = componentMap[ext];
        if (!component) throw new Error('Unsupported: .' + ext);

        const src = ref(arrayBuffer);
        const self = this;

        const app = createApp({
            setup() {
                return () => h(component, {
                    src: src.value,
                    style: ext === 'xlsx' || ext === 'xls'
                        ? 'width: 100%; height: 100%;'
                        : 'width: 100%; min-height: 100%;',
                    onRendered: () => {
                        nextTick(() => {
                            setTimeout(() => {
                                if (ext === 'docx') {
                                    const pages = getDocxPages(container);
                                    if (callbacks && callbacks.onRendered) callbacks.onRendered({ pages: pages.length, type: ext });
                                    setupPageObserver(container, pages, callbacks);
                                } else if (ext === 'pptx') {
                                    const slides = getPptxSlides(container);
                                    if (callbacks && callbacks.onRendered) callbacks.onRendered({ pages: slides.length, type: ext });
                                } else {
                                    if (callbacks && callbacks.onRendered) callbacks.onRendered({ pages: 0, type: ext });
                                }
                            }, 400);
                        });
                    },
                    onError: (err) => {
                        if (callbacks && callbacks.onError) callbacks.onError(err);
                    }
                });
            }
        });

        app.mount(container);
        currentApp = app;

        return {
            destroy() { self.destroy(); },
            goToSlide(idx) {
                const slides = getPptxSlides(container);
                if (slides[idx]) slides[idx].scrollIntoView({ behavior: 'smooth', block: 'start' });
            },
            goToPage(idx) {
                const pages = getDocxPages(container);
                if (pages[idx]) pages[idx].scrollIntoView({ behavior: 'smooth', block: 'start' });
            },
            getSlideCount() { return getPptxSlides(container).length; },
            getPageCount() { return getDocxPages(container).length; },
        };
    },

    destroy() {
        if (pageObserver) { pageObserver.disconnect(); pageObserver = null; }
        if (currentApp) { try { currentApp.unmount(); } catch (e) {} currentApp = null; }
    }
};
