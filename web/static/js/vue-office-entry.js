import { createApp, h, ref, nextTick, onMounted, onUnmounted, watch } from 'vue';
import VueOfficeDocx from '@vue-office/docx/lib/v3/vue-office-docx.mjs';
import VueOfficeExcel from '@vue-office/excel/lib/v3/vue-office-excel.mjs';
import VueOfficePptx from '@vue-office/pptx/lib/v3/vue-office-pptx.mjs';

import '@vue-office/docx/lib/v3/index.css';
import '@vue-office/excel/lib/v3/index.css';

let currentApp = null;
let currentSlides = [];
let currentSlideIdx = 0;

function countDocxPages(container) {
    const pages = container.querySelectorAll('.docx-wrapper > section, .docx-wrapper > .page, .docx-wrapper > div[class*="page"]');
    return pages.length || container.querySelectorAll('.docx-wrapper > *').length || 1;
}

function countPptxSlides(container) {
    const slides = container.querySelectorAll('.pptx-preview-wrapper > div, [class*="slide"], [class*="pptx"]');
    if (slides.length > 0) return slides.length;
    const allDivs = container.querySelectorAll('.pptx-preview-wrapper > *');
    return allDivs.length || 1;
}

function scrollToSlide(container, idx) {
    const slides = container.querySelectorAll('.pptx-preview-wrapper > div, [class*="slide"], [class*="pptx"]');
    const target = slides.length > 0 ? slides[idx] : container.querySelectorAll('.pptx-preview-wrapper > *')[idx];
    if (target) {
        target.scrollIntoView({ behavior: 'smooth', block: 'start' });
        currentSlideIdx = idx;
    }
}

window.VueOffice = {
    render(container, type, arrayBuffer, callbacks) {
        this.destroy();
        const ext = type.toLowerCase();

        const componentMap = {
            docx: VueOfficeDocx,
            xlsx: VueOfficeExcel,
            xls: VueOfficeExcel,
            pptx: VueOfficePptx,
        };

        const component = componentMap[ext];
        if (!component) {
            throw new Error('Unsupported: .' + ext);
        }

        const src = ref(arrayBuffer);
        const self = this;

        const app = createApp({
            setup() {
                onMounted(() => {
                    nextTick(() => {
                        setTimeout(() => {
                            const pages = ext === 'docx' ? countDocxPages(container) :
                                          ext === 'pptx' ? countPptxSlides(container) : 0;
                            if (callbacks && callbacks.onRendered) {
                                callbacks.onRendered({ pages, type: ext });
                            }
                        }, 500);
                    });
                });

                return () => h(component, {
                    src: src.value,
                    style: 'width: 100%; min-height: 100%;',
                    onRendered: () => {
                        nextTick(() => {
                            setTimeout(() => {
                                const pages = ext === 'docx' ? countDocxPages(container) :
                                              ext === 'pptx' ? countPptxSlides(container) : 0;
                                if (callbacks && callbacks.onRendered) {
                                    callbacks.onRendered({ pages, type: ext });
                                }
                            }, 300);
                        });
                    },
                    onError: (err) => {
                        if (callbacks && callbacks.onError) {
                            callbacks.onError(err);
                        }
                    }
                });
            }
        });

        app.mount(container);
        currentApp = app;
        currentSlides = [];
        currentSlideIdx = 0;

        return {
            destroy() { self.destroy(); },
            goToSlide(idx) { scrollToSlide(container, idx); currentSlideIdx = idx; },
            getSlideCount() { return countPptxSlides(container); },
            getCurrentSlide() { return currentSlideIdx; },
        };
    },

    destroy() {
        if (currentApp) {
            try { currentApp.unmount(); } catch (e) {}
            currentApp = null;
        }
        currentSlides = [];
        currentSlideIdx = 0;
    }
};
