import { async, ComponentFixture, TestBed } from '@angular/core/testing';

import { ContentSwitcherComponent } from './content-switcher.component';
import { OverviewModule } from '../../overview.module';

describe('ContentSwitcherComponent', () => {
  let component: ContentSwitcherComponent;
  let fixture: ComponentFixture<ContentSwitcherComponent>;

  beforeEach(async(() => {
    TestBed.configureTestingModule({
      imports: [OverviewModule],
    })
    .compileComponents();
  }));

  beforeEach(() => {
    fixture = TestBed.createComponent(ContentSwitcherComponent);
    component = fixture.componentInstance;
    fixture.detectChanges();
  });

  it('should create', () => {
    expect(component).toBeTruthy();
  });
});
